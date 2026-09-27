package shopper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/botclient"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

const (
	// fundingTimeout is how long we wait for a funding to confirm before giving up on the order.
	fundingTimeout = 24 * time.Hour
	// fundingGrace is how long after expires_at a funding may still confirm (spec §4.6).
	fundingGrace = int64(3600)
)

// purchaseRetry is the pause after a purchase call that has to be repeated.
var purchaseRetry = 5 * time.Second

func (e *Engine) onFunded(ctx context.Context, msg *messenger.Message) {
	var f proto.OrderFunded
	if err := msg.Decode(&f); err != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		// only an accepted quote is funded; an order.funded that overtakes the accept is replayed by onAccept
		if o.User != msg.From || o.State != StateAccepted || o.Events["accept"] == nil {
			return errSkip
		}
		if f.Asset != o.Quote.Asset {
			return errSkip
		}
		o.Funded = &f
		o.Events["funded"] = msg.Inner
		o.FundingSince = time.Now().Unix()
		o.set(StateFunding, "waiting for confirmations")
		return nil
	})
	if err == nil {
		e.background(ctx, "verify", msg.OrderID, func(ctx context.Context) { e.verifyFunding(ctx, msg.OrderID) })
	}
}

// checkFunding verifies the orders waiting for their funding and starts (or resumes) their purchases.
func (e *Engine) checkFunding(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	for _, o := range orders {
		id := o.ID
		switch o.State {
		case StateFunding:
			e.background(ctx, "verify", id, func(ctx context.Context) { e.verifyFunding(ctx, id) })
		case StateFunded, StatePurchasing:
			// resume after a restart or a failed call; the bot answers a repeated request_id from its records
			e.background(ctx, "purchase", id, func(ctx context.Context) { e.startPurchase(ctx, id) })
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
	f, err := e.checkChain(vctx, o)
	if err == nil {
		err = e.claimUses(id, f.Uses)
	}
	switch {
	case err == nil:
		late := f.ConfirmedAt > e.fundingDeadline(o)
		o, err = e.update(id, func(o *Order) error {
			if o.State != StateFunding {
				return errSkip
			}
			if o.Quote.Asset == proto.AssetBTC {
				op := f.Outpoint
				o.Outpoint = &op
				o.set(StateFunded, fmt.Sprintf("%s:%d %d sats", op.TxID, op.Vout, op.Amount))
			} else {
				o.Safe = f.Safe.Hex()
				o.set(StateFunded, "safe "+o.Safe)
			}
			return nil
		})
		if err != nil {
			return
		}
		if late {
			// the price and the rate of the quote no longer hold; give the money back instead of buying
			e.abandon(ctx, id, "funded after the quote expired")
			return
		}
		e.log.Info("funding verified", "order", id)
		e.background(ctx, "purchase", id, func(ctx context.Context) { e.startPurchase(ctx, id) })
	case contract.IsDefinite(err):
		// a wrong funding stays wrong; tell the user and wait for a corrected order.funded
		e.log.Warn("funding rejected", "order", id, "err", err)
		_, _ = e.update(id, func(o *Order) error {
			o.Error = err.Error()
			o.set(StateAccepted, "funding rejected: "+err.Error())
			return nil
		})
		_, _ = e.send(ctx, o, o.User, proto.TypeChat, proto.Chat{Text: "funding rejected: " + err.Error()})
	default:
		// not confirmed yet, or the chain could not be asked: keep waiting
		_, _ = e.update(id, func(o *Order) error {
			if time.Since(time.Unix(o.FundingSince, 0)) > fundingTimeout {
				o.set(StateCancelled, "funding never confirmed")
				return nil
			}
			if !errors.Is(err, contract.ErrNotYet) {
				o.note("funding check failed, will retry: " + err.Error())
			}
			return nil
		})
	}
}

// checkChain runs the funding checks of §4.6 for the asset of the order.
func (e *Engine) checkChain(ctx context.Context, o *Order) (contract.Funding, error) {
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return contract.Funding{}, contract.Definite(err)
		}
		return contract.VerifyBTCFunding(ctx, e.BTC, esc, o.Quote, o.Funded, e.cfg.Confirmations)
	case proto.AssetUSDC:
		d, err := e.Deployments()
		if err != nil {
			return contract.Funding{}, err
		}
		os, err := contract.SafeOf(d, &o.Request, o.Quote)
		if err != nil {
			return contract.Funding{}, contract.Definite(err)
		}
		return contract.VerifySafeFunding(ctx, e.EVM, d, os, o.ID, &o.Request, o.Quote, o.Funded, e.cfg.Confirmations)
	}
	return contract.Funding{}, contract.Mismatchf("unknown asset %q", o.Quote.Asset)
}

// fundingDeadline is the latest block time of a funding we still buy for: expires_at plus the grace, on the
// clock of the chain (the EVM clock of the lab may run ahead).
func (e *Engine) fundingDeadline(o *Order) int64 {
	exp := o.Quote.ExpiresAt
	if o.Quote.Asset == proto.AssetUSDC && o.ChainExpiresAt != 0 {
		exp = o.ChainExpiresAt
	}
	return exp + fundingGrace
}

// claimUses records that the chain objects of a funding belong to this order. One outpoint, Safe or fee
// transaction can fund only one order.
func (e *Engine) claimUses(id string, uses []string) error {
	for _, u := range uses {
		err := store.Modify(e.db, bucketUses, u, func(owner *string, exists bool) error {
			if exists && *owner != id {
				return contract.Mismatchf("%s already funds order %s", u, *owner)
			}
			if exists {
				return store.ErrStop
			}
			*owner = id
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// timeToT1 is how many seconds are left until T1 (BTC: 600 s per block).
func (e *Engine) timeToT1(ctx context.Context, o *Order) (int64, error) {
	switch o.Quote.Asset {
	case proto.AssetBTC:
		tip, err := e.BTC.TipHeight(ctx)
		if err != nil {
			return 0, err
		}
		return (o.Quote.Timelock.T1 - tip) * 600, nil
	case proto.AssetUSDC:
		now, err := e.EVM.LatestTime(ctx)
		if err != nil {
			return 0, err
		}
		return o.Quote.Timelock.T1 - int64(now), nil
	}
	return 0, fmt.Errorf("unknown asset %q", o.Quote.Asset)
}

// startPurchase buys the items through shopper-bot. The request_id is always the order id: the bot keeps one
// result per request_id, so asking again after a restart or a lost answer returns that result instead of
// buying twice (spec §9).
func (e *Engine) startPurchase(ctx context.Context, id string) {
	o, ok, err := e.Order(id)
	if err != nil || !ok {
		return
	}
	switch o.State {
	case StateFunded:
		// before the first call only: enough time must be left to buy and deliver before T1
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		left, err := e.timeToT1(tctx, o)
		cancel()
		if err != nil {
			e.fail(id, "time to T1", err)
			return
		}
		if left < e.cfg.MinT1RemainingSeconds {
			e.abandon(ctx, id, fmt.Sprintf("only %d s left until T1, %d s needed", left, e.cfg.MinT1RemainingSeconds))
			return
		}
		if o, err = e.update(id, func(o *Order) error {
			if o.State != StateFunded {
				return errSkip
			}
			o.set(StatePurchasing, "")
			return nil
		}); err != nil {
			return
		}
	case StatePurchasing:
	default:
		return
	}
	e.purchaseOnce(ctx, o)
}

// purchaseOnce calls the bot for a purchasing order and records the answer. The order may have moved on while
// the call ran (a dispute, a payout); then the result is recorded without changing the state.
func (e *Engine) purchaseOnce(ctx context.Context, o *Order) {
	id := o.ID
	addr, err := e.openAddress(o)
	if err != nil {
		e.purchaseFailed(ctx, o.ID, fmt.Errorf("delivery address: %w", err))
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
	switch {
	case botclient.InProgress(err):
		// the bot is still buying this request_id; ask again later
		_, _ = e.update(id, func(o *Order) error { o.note("the bot is still buying"); return nil })
		time.Sleep(purchaseRetry)
		return
	case botclient.Rejected(err):
		e.purchaseFailed(ctx, id, err)
		return
	case err != nil:
		// the bot may be restarting; keep the order in purchasing and ask again with the same request_id
		e.fail(id, "purchase call failed", err)
		time.Sleep(purchaseRetry)
		return
	}
	switch res.Status {
	case "ok":
		if err := e.db.Put(bucketEvidence, id, res.Evidence); err != nil {
			e.fail(id, "store purchase evidence", err)
			return
		}
		body := proto.OrderPurchased{ShopOrderID: res.ShopOrderID, Evidence: proto.InlineOnly(res.Evidence)}
		if res.Total != nil {
			body.Total = *res.Total
		}
		ev, err := e.send(ctx, o, o.User, proto.TypeOrderPurchased, body)
		if err != nil {
			e.fail(id, "purchased not sent", err)
		}
		_, _ = e.update(id, func(o *Order) error {
			o.Purchase = withoutData(res)
			if ev != nil {
				o.Events["purchased"] = ev
			}
			// a dispute or payout that happened meanwhile keeps its state; the purchase is recorded anyway
			if o.State == StatePurchasing {
				o.set(StatePurchased, res.ShopOrderID)
			} else {
				o.note("purchased " + res.ShopOrderID)
			}
			return nil
		})
		e.log.Info("purchased", "order", id, "shop_order", res.ShopOrderID)
	case "needs_human":
		_, _ = e.update(id, func(o *Order) error {
			o.Purchase = withoutData(res)
			if o.State == StatePurchasing {
				o.set(StateNeedsHuman, res.Error)
			} else {
				o.note("bot needs a human: " + res.Error)
			}
			return nil
		})
	default:
		e.purchaseFailed(ctx, id, fmt.Errorf("bot: %s", res.Error))
	}
}

// withoutData copies a purchase result without the evidence data; the full items stay in bucketEvidence.
func withoutData(res *proto.PurchaseResult) *proto.PurchaseResult {
	out := *res
	out.Evidence = make([]proto.Evidence, len(res.Evidence))
	for i, ev := range res.Evidence {
		ev.DataB64 = ""
		out.Evidence[i] = ev
	}
	return &out
}

// PurchaseEvidence returns the full evidence of the purchase (with the data of screenshots and receipts).
func (e *Engine) PurchaseEvidence(id string) []proto.Evidence {
	var evs []proto.Evidence
	if ok, _ := e.db.Get(bucketEvidence, id, &evs); ok {
		return evs
	}
	if o, ok, _ := e.Order(id); ok && o.Purchase != nil {
		return o.Purchase.Evidence // orders stored before the evidence bucket existed
	}
	return nil
}

func (e *Engine) purchaseFailed(ctx context.Context, id string, cause error) {
	e.log.Warn("purchase failed", "order", id, "err", cause)
	e.abandon(ctx, id, cause.Error())
}

// abandon ends an order we will not (or can no longer) buy: purchase_failed, with a cooperative refund offered.
// Orders that moved on (dispute, payout) keep their state.
func (e *Engine) abandon(ctx context.Context, id, reason string) {
	_, err := e.update(id, func(o *Order) error {
		switch o.State {
		case StateFunded, StatePurchasing, StateNeedsHuman:
			o.set(StatePurchaseFailed, reason)
		default:
			o.note("not buying: " + reason)
			return nil
		}
		if o.funded() {
			o.addPending(ActRefund, &Action{})
		}
		return nil
	})
	if err == nil {
		e.kick(ctx, id)
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
