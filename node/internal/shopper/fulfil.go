package shopper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// fundingTimeout is how long we wait for a funding to confirm before giving up on the order.
const fundingTimeout = 24 * time.Hour

func (e *Engine) onFunded(ctx context.Context, msg *messenger.Message) {
	var f proto.OrderFunded
	if err := msg.Decode(&f); err != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		if o.User != msg.From || (o.State != StateAccepted && o.State != StateQuoted) {
			return errSkip
		}
		if o.Events["accept"] == nil {
			// order.accept may still be on its way; the funded message implies it
			o.note("funded before accept arrived")
		}
		if f.Asset != o.Quote.Asset {
			return errSkip
		}
		o.Funded = &f
		o.Events["funded"] = msg.Inner
		o.set(StateFunding, "waiting for confirmations")
		return nil
	})
	if err == nil {
		e.checkFunding(ctx)
	}
}

// checkFunding verifies the orders waiting for their funding and starts the purchase of verified ones.
func (e *Engine) checkFunding(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	for _, o := range orders {
		switch o.State {
		case StateFunding:
			e.verifyFunding(ctx, o.ID)
		case StateFunded, StatePurchasing:
			// resume after a restart
			e.startPurchase(ctx, o.ID)
		}
	}
}

func (e *Engine) verifyFunding(ctx context.Context, id string) {
	unlock := e.lock(id)
	defer unlock()
	o, ok, err := e.Order(id)
	if err != nil || !ok || o.State != StateFunding {
		return
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var outpointErr error
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			outpointErr = err
			break
		}
		op, err := contract.VerifyBTCFunding(vctx, e.BTC, esc, o.Quote, o.Funded, e.cfg.Confirmations)
		if err == nil {
			_, err = e.update(id, func(o *Order) error {
				o.Outpoint = &op
				o.set(StateFunded, fmt.Sprintf("%s:%d %d sats", op.TxID, op.Vout, op.Amount))
				return nil
			})
		}
		outpointErr = err
	case proto.AssetUSDC:
		d, err := e.Deployments()
		if err != nil {
			outpointErr = err
			break
		}
		os, err := contract.SafeOf(d, &o.Request, o.Quote)
		if err != nil {
			outpointErr = err
			break
		}
		safe, err := contract.VerifySafeFunding(vctx, e.EVM, d, os, o.ID, o.Quote, o.Funded, e.cfg.Confirmations)
		if err == nil {
			_, err = e.update(id, func(o *Order) error {
				o.Safe = safe.Hex()
				o.set(StateFunded, "safe "+safe.Hex())
				return nil
			})
		}
		outpointErr = err
	}
	switch {
	case outpointErr == nil:
		e.log.Info("funding verified", "order", id)
		go e.startPurchase(context.WithoutCancel(ctx), id)
	case errors.Is(outpointErr, contract.ErrNotYet) || isTransient(outpointErr):
		if time.Since(time.Unix(o.Updated, 0)) > fundingTimeout {
			_, _ = e.update(id, func(o *Order) error { o.set(StateCancelled, "funding never confirmed"); return nil })
		}
	default:
		// a wrong funding stays wrong; tell the user and wait for a corrected order.funded
		e.log.Warn("funding rejected", "order", id, "err", outpointErr)
		_, _ = e.update(id, func(o *Order) error {
			o.Error = outpointErr.Error()
			o.set(StateAccepted, "funding rejected: "+outpointErr.Error())
			return nil
		})
		_, _ = e.send(ctx, o, o.User, proto.TypeChat, proto.Chat{Text: "funding rejected: " + outpointErr.Error()})
	}
}

// isTransient tells network trouble from a wrong funding.
func isTransient(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// startPurchase buys the items through shopper-bot once per order.
func (e *Engine) startPurchase(ctx context.Context, id string) {
	if _, running := e.busy.LoadOrStore(id, struct{}{}); running {
		return
	}
	defer e.busy.Delete(id)
	o, err := e.update(id, func(o *Order) error {
		if o.State != StateFunded && o.State != StatePurchasing {
			return errSkip
		}
		if o.State == StateFunded {
			o.set(StatePurchasing, "")
		}
		return nil
	})
	if err != nil {
		return
	}
	k, err := delivery.UnwrapKey(e.Keys.NostrSecretHex(), o.User, o.Request.Delivery.KeyForShopper)
	if err != nil {
		e.purchaseFailed(ctx, o, fmt.Errorf("delivery key: %w", err))
		return
	}
	addr, err := delivery.Open(k, o.Request.Delivery.Ciphertext, o.ID)
	if err != nil {
		e.purchaseFailed(ctx, o, err)
		return
	}
	payment := "card:default"
	if o.Shop != nil && o.Shop.CashOnly {
		payment = "cash"
	}
	max := o.Quote.Price.Items
	if total, err := sumMoney(o.Quote.Price.Items, o.Quote.Price.Shipping); err == nil {
		max = total
	}
	res, err := e.Bot.Purchase(ctx, proto.PurchaseRequest{
		RequestID: o.ID, OrderID: o.ID, ShopURL: o.Request.ShopURL, Items: o.Request.Items,
		Shipping: addr, PaymentRef: payment, MaxAmount: max,
	})
	if err != nil {
		// the bot may be restarting; keep the order in purchasing and retry on the next funding tick
		e.fail(id, "purchase call failed", err)
		time.Sleep(5 * time.Second)
		return
	}
	switch res.Status {
	case "ok":
		body := proto.OrderPurchased{ShopOrderID: res.ShopOrderID, Evidence: proto.InlineOnly(res.Evidence)}
		if res.Total != nil {
			body.Total = *res.Total
		}
		ev, err := e.send(ctx, o, o.User, proto.TypeOrderPurchased, body)
		if err != nil {
			e.fail(id, "purchased not sent", err)
		}
		_, _ = e.update(id, func(o *Order) error {
			o.Purchase = res
			if ev != nil {
				o.Events["purchased"] = ev
			}
			o.set(StatePurchased, res.ShopOrderID)
			return nil
		})
		e.log.Info("purchased", "order", id, "shop_order", res.ShopOrderID)
	case "needs_human":
		_, _ = e.update(id, func(o *Order) error { o.Purchase = res; o.set(StateNeedsHuman, res.Error); return nil })
	default:
		e.purchaseFailed(ctx, o, fmt.Errorf("bot: %s", res.Error))
	}
}

func (e *Engine) purchaseFailed(ctx context.Context, o *Order, cause error) {
	e.log.Warn("purchase failed", "order", o.ID, "err", cause)
	_, _ = e.update(o.ID, func(o *Order) error { o.set(StatePurchaseFailed, cause.Error()); return nil })
	// offer the money back: a cooperative refund signed by us (order.refund)
	if err := e.offerRefund(ctx, o.ID); err != nil {
		e.log.Warn("refund offer failed", "order", o.ID, "err", err)
	}
}

func sumMoney(a, b proto.Money) (proto.Money, error) {
	if a.Currency != b.Currency {
		return proto.Money{}, errors.New("different currencies")
	}
	x, err := parseRat(a.Amount)
	if err != nil {
		return proto.Money{}, err
	}
	y, err := parseRat(b.Amount)
	if err != nil {
		return proto.Money{}, err
	}
	return proto.Money{Amount: formatCur(x.Add(x, y), a.Currency), Currency: a.Currency}, nil
}

// pollTracking asks the bot for the shipping state of purchased orders and reports changes.
func (e *Engine) pollTracking(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	for _, o := range orders {
		if o.Purchase == nil || o.Purchase.ShopOrderID == "" || terminal(o.State) {
			continue
		}
		if o.ShipStatus == "delivered" || o.ShipStatus == "failed" {
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		st, err := e.Bot.Tracking(tctx, proto.TrackingQuery{ShopURL: o.Request.ShopURL, ShopOrderID: o.Purchase.ShopOrderID})
		cancel()
		if err != nil {
			e.log.Debug("tracking failed", "order", o.ID, "err", err)
			continue
		}
		if st.Status == o.ShipStatus || st.Status == "processing" {
			continue
		}
		e.reportShipping(ctx, o.ID, st)
	}
}

func (e *Engine) reportShipping(ctx context.Context, id string, st *proto.TrackingStatus) {
	defer e.lock(id)()
	o, err := e.update(id, func(o *Order) error {
		if o.ShipStatus == st.Status {
			return errSkip
		}
		o.ShipStatus = st.Status
		o.Tracking = append(o.Tracking, *st)
		// the order state follows the shipping unless the order moved on (dispute, payout)
		switch {
		case terminal(o.State) || o.State == StateDisputed:
		case st.Status == "shipped":
			o.set(StateShipped, st.TrackingNo)
		case st.Status == "delivered":
			o.set(StateDelivered, st.TrackingNo)
		case st.Status == "failed":
			o.set(StateShipFailed, "")
		}
		return nil
	})
	if err != nil {
		return
	}
	if _, err := e.send(ctx, o, o.User, proto.TypeOrderShipping, proto.OrderShipping{Status: st.Status, Tracking: *st}); err != nil {
		e.fail(id, "shipping not sent", err)
	}
	e.log.Info("shipping", "order", id, "status", st.Status)
}
