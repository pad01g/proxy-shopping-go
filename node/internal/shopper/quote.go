package shopper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/amount"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/fx"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shop"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// rejection is a quote refusal with its reason.
type rejection struct {
	reason, detail string
}

func (r *rejection) Error() string { return r.reason + ": " + r.detail }

func reject(reason, format string, args ...any) error {
	return &rejection{reason: reason, detail: fmt.Sprintf(format, args...)}
}

func (e *Engine) onRequest(ctx context.Context, msg *messenger.Message) {
	if _, err := keys.OrderIDBytes(msg.OrderID); err != nil {
		e.log.Warn("request without a valid order id", "from", msg.From)
		return
	}
	var req proto.OrderRequest
	if err := msg.Decode(&req); err != nil {
		e.log.Warn("bad request", "order", msg.OrderID, "err", err)
		return
	}
	o := &Order{
		ID: msg.OrderID, User: msg.From, Created: time.Now().Unix(), Request: req, Relays: req.Relays,
		Events: map[string]*nostr.Event{"request": msg.Inner},
	}
	if req.ReplyP2P != nil {
		// optional: a bad one only costs the P2P path, replies go to the mailbox (§4.2)
		if err := req.ReplyP2P.Validate(); err != nil {
			e.log.Info("ignoring the reply_p2p of a request", "order", msg.OrderID, "err", err)
			o.note("reply_p2p ignored: " + err.Error())
		} else {
			o.ReplyP2P = req.ReplyP2P
		}
	}
	o.set(StateRequested, "")
	o.addPending(ActQuote, &Action{})
	// an order id is used once; a repeated request of the same user gets the stored answer again
	existing, ok, _ := e.Order(o.ID)
	if ok {
		if existing.User == msg.From && existing.Events["quote"] != nil {
			e.log.Info("request repeated, quote already sent", "order", o.ID)
		}
		return
	}
	if err := e.db.Put(bucketOrders, o.ID, o); err != nil {
		e.log.Error("store order", "err", err)
		return
	}
	// relays do not keep order: the escrow key may have come first
	e.replay(ctx, o.ID, o.User, proto.TypeOrderEscrowKey, e.onEscrowKey)
	// quoting fetches the shop and the rates; the pending quote is worked off (and retried) in the background
	e.kick(ctx, o.ID)
}

// replay hands the latest stored message of a type from the user to its handler again. It is used when a message
// arrived before the one it builds on (relays do not keep order), and was therefore skipped.
func (e *Engine) replay(ctx context.Context, id, from, typ string, h messenger.Handler) {
	var last *nostr.Event
	for _, ev := range e.inbox(id) {
		if ev.PubKey == from && giftwrap.Type(ev) == typ && (last == nil || ev.CreatedAt >= last.CreatedAt) {
			last = ev
		}
	}
	if last != nil {
		h(ctx, &messenger.Message{Inner: last, From: from, Type: typ, OrderID: id})
	}
}

// onEscrowKey keeps key_for_escrow for a dispute, after checking it against the request's commitment.
func (e *Engine) onEscrowKey(ctx context.Context, msg *messenger.Message) {
	var k proto.EscrowKey
	if msg.Decode(&k) != nil || k.KeyForEscrow == "" {
		return
	}
	_, _ = e.update(msg.OrderID, func(o *Order) error {
		if o.User != msg.From || o.EscrowKey != "" {
			return errSkip
		}
		if proto.EscrowKeyHash(k.KeyForEscrow) != o.Request.Delivery.KeyForEscrowSHA256 {
			o.note("order.escrow_key does not match key_for_escrow_sha256 of the request, ignored")
			return nil
		}
		o.EscrowKey = k.KeyForEscrow
		o.Events["escrow_key"] = msg.Inner
		o.note("escrow key received")
		return nil
	})
}

// doQuote answers a request (the pending quote action). The quote is stored with its state before it is sent,
// so that an order.accept arriving right after it finds the order quoted, and a quote that could not be sent is
// sent again as it was. The messenger keeps a sent message in its outbox before it leaves: a quote an earlier
// attempt sent (and then crashed) is found there, and no second quote is made.
func (e *Engine) doQuote(ctx context.Context, o *Order) error {
	if o.Events["quote"] != nil {
		return nil
	}
	if ev := e.sentQuote(o); ev != nil {
		return e.recordQuote(ctx, o.ID, ev)
	}
	q := o.Quote
	if o.State == StateRequested {
		var info *shop.Info
		var score int
		var err error
		q, info, score, err = e.buildQuote(ctx, o)
		if err != nil {
			var rj *rejection
			if !errors.As(err, &rj) {
				e.log.Warn("quote failed", "order", o.ID, "err", err)
				rj = &rejection{reason: proto.RejectUnavailable, detail: "the quote could not be made"}
			}
			q = &proto.OrderQuote{Accept: false, RejectReason: rj.reason, Detail: rj.detail}
		}
		var don *Donation
		if q.Accept {
			o.Shop = info
			if btcAddr, evmAddr, bps := e.donation(o); bps > 0 {
				don = &Donation{BTCAddress: btcAddr, EVMAddress: evmAddr, BPS: bps}
			}
		}
		if _, err := e.update(o.ID, func(cur *Order) error {
			if cur.State != StateRequested {
				return errSkip
			}
			cur.Quote, cur.Shop, cur.Score, cur.ChainExpiresAt, cur.Donation = q, info, score, o.ChainExpiresAt, don
			if q.Accept {
				cur.set(StateQuoted, fmt.Sprintf("lock %s %s", q.LockAmount, q.Asset))
			} else {
				cur.set(StateRejected, q.RejectReason+": "+q.Detail)
			}
			return nil
		}); err != nil {
			if errors.Is(err, errSkip) {
				return nil
			}
			return err
		}
	}
	if q == nil {
		return contract.Mismatchf("no quote to send in state %s", o.State)
	}
	ev, err := e.send(ctx, o, o.User, proto.TypeOrderQuote, q)
	if err != nil {
		return err
	}
	e.log.Info("quoted", "order", o.ID, "accept", q.Accept, "reason", q.RejectReason, "detail", q.Detail)
	return e.recordQuote(ctx, o.ID, ev)
}

// sentQuote finds an order.quote of ours for the order in the messenger's outbox (the latest).
func (e *Engine) sentQuote(o *Order) *nostr.Event {
	var last *nostr.Event
	for _, ev := range e.Messenger.Outbox(o.ID) {
		if giftwrap.Type(ev) == proto.TypeOrderQuote && giftwrap.Recipient(ev) == o.User && (last == nil || ev.CreatedAt >= last.CreatedAt) {
			last = ev
		}
	}
	return last
}

// recordQuote stores the quote message that was sent, and looks again at an order.accept that came before.
func (e *Engine) recordQuote(ctx context.Context, id string, ev *nostr.Event) error {
	o, err := e.update(id, func(o *Order) error {
		if o.Events["quote"] != nil {
			return errSkip
		}
		o.Events["quote"] = ev
		if o.Quote == nil || o.State == StateRequested {
			// sent by an attempt that crashed before the state was stored
			var q proto.OrderQuote
			if json.Unmarshal([]byte(ev.Content), &q) == nil {
				o.Quote = &q
				if q.Accept {
					o.set(StateQuoted, fmt.Sprintf("lock %s %s", q.LockAmount, q.Asset))
				} else {
					o.set(StateRejected, q.RejectReason+": "+q.Detail)
				}
			}
		}
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil {
		return err
	}
	if o.State == StateQuoted {
		e.replay(ctx, id, o.User, proto.TypeOrderAccept, e.onAccept)
	}
	return nil
}

// WantsRetry tells the messenger whether unacknowledged messages about an order are resent: to the user of an
// order we keep, but not for a rejected request, which is answered once (§4.10).
func (e *Engine) WantsRetry(orderID string) bool {
	o, ok, err := e.Order(orderID)
	return err == nil && ok && o.State != StateRejected
}

// Limits of the bot schema (shopper-bot/schema) checked before quoting.
const (
	maxItems   = 20
	maxQty     = 99
	maxSKU     = 64
	maxAddress = 500
)

// buildQuote runs the checks of §4.5 / §8 and prices the order.
func (e *Engine) buildQuote(ctx context.Context, o *Order) (*proto.OrderQuote, *shop.Info, int, error) {
	req := &o.Request
	me := e.Keys.NostrPubHex()

	// payment method
	if !slices.Contains(e.cfg.Payments, req.Payment) {
		return nil, nil, 0, reject(proto.RejectPayment, "%s is not accepted", req.Payment)
	}
	switch req.Payment {
	case proto.AssetBTC:
		if e.BTC == nil {
			return nil, nil, 0, reject(proto.RejectPayment, "no bitcoin backend")
		}
		if _, err := keys.ParsePubKeyHex(req.UserBTCPubkey); err != nil {
			return nil, nil, 0, reject(proto.RejectInvalid, "user_btc_pubkey: %v", err)
		}
		if _, err := btc.PkScript(req.UserBTCAddress); err != nil {
			return nil, nil, 0, reject(proto.RejectInvalid, "user_btc_address: %v", err)
		}
	case proto.AssetUSDC:
		if e.EVM == nil || e.Deployments == nil {
			return nil, nil, 0, reject(proto.RejectPayment, "no EVM backend")
		}
		if !common.IsHexAddress(req.UserEVMAddress) {
			return nil, nil, 0, reject(proto.RejectInvalid, "user_evm_address is not an address")
		}
	default:
		return nil, nil, 0, reject(proto.RejectPayment, "unknown payment %q", req.Payment)
	}
	if err := contract.VerifyKeyProof(o.ID, o.User, req); err != nil {
		return nil, nil, 0, reject(proto.RejectInvalid, "%v", err)
	}
	u, err := shop.ParseURL(req.ShopURL)
	if err != nil {
		return nil, nil, 0, reject(proto.RejectInvalid, "%v", err)
	}
	host := strings.ToLower(u.Hostname())
	if len(req.Items) == 0 || len(req.Items) > maxItems {
		return nil, nil, 0, reject(proto.RejectInvalid, "1 to %d items are needed", maxItems)
	}
	for _, it := range req.Items {
		if it.SKU == "" || len(it.SKU) > maxSKU || it.Qty < 1 || it.Qty > maxQty {
			return nil, nil, 0, reject(proto.RejectInvalid, "item %.64q: sku of 1-%d bytes and qty 1-%d are needed", it.SKU, maxSKU, maxQty)
		}
	}
	if req.Delivery.Ciphertext == "" || req.Delivery.KeyForShopper == "" || !isHex32(req.Delivery.KeyForEscrowSHA256) {
		return nil, nil, 0, reject(proto.RejectInvalid, "delivery ciphertext, key_for_shopper and key_for_escrow_sha256 are required")
	}

	// the shopper × escrow combination must be in the effective set and cover the shop region
	rows := e.Trust.Effective(e.Coordinators, e.Network)
	var mine []trust.Row
	for _, r := range rows {
		if r.Shopper == me && r.Escrow == req.Escrow {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return nil, nil, 0, reject(proto.RejectTrust, "shopper %s with escrow %s is not in the effective list", me[:12], short(req.Escrow))
	}
	match := trust.Find(rows, trust.Match{Shopper: me, Escrow: req.Escrow, Region: req.ShopRegion}, req.Operator)
	if len(match) == 0 {
		return nil, nil, 0, reject(proto.RejectRegion, "no list entry covers %s", req.ShopRegion)
	}
	match = trust.Find(match, trust.Match{Shopper: me, Escrow: req.Escrow, Region: req.ShopRegion, Payment: req.Payment}, req.Operator)
	if len(match) == 0 {
		return nil, nil, 0, reject(proto.RejectPayment, "%s is not listed for this combination", req.Payment)
	}
	match = trust.Find(match, trust.Match{Shopper: me, Escrow: req.Escrow, Region: req.ShopRegion, Payment: req.Payment, Host: host}, req.Operator)
	if len(match) == 0 {
		return nil, nil, 0, reject(proto.RejectRegion, "shop %s is not listed for this combination", host)
	}
	// the operator the request names (its list decides the donation, §2.3) must be the one listing us
	if !slices.ContainsFunc(match, func(r trust.Row) bool { return r.Operator == req.Operator }) {
		return nil, nil, 0, reject(proto.RejectTrust, "operator %s does not list this combination", short(req.Operator))
	}
	escrow, _ := e.Trust.EscrowProfile(req.Escrow, e.Network)
	if escrow == nil {
		return nil, nil, 0, reject(proto.RejectUnavailable, "no profile of escrow %s", short(req.Escrow))
	}
	// the address must open and be something the bot can use (shopper-bot/schema/Address.json)
	if err := e.checkAddress(o); err != nil {
		return nil, nil, 0, reject(proto.RejectInvalid, "delivery address: %v", err)
	}

	// shop risk (§8)
	info, err := e.Shops.Inspect(ctx, req.ShopURL)
	if err != nil {
		// the error may describe the shopper's own network; the requester only learns that it failed
		e.log.Info("shop not reachable", "order", o.ID, "url", req.ShopURL, "err", err)
		return nil, nil, 0, reject(proto.RejectUnavailable, "shop not reachable")
	}
	score := 0
	if info.CashOnly {
		region := info.Region
		if region == "" {
			region = req.ShopRegion
		}
		if !trust.CoversAny(e.cfg.CashRegions, region) {
			return nil, info, 0, reject(proto.RejectRegion, "cash-only shop in %s is outside our cash regions %v", region, e.cfg.CashRegions)
		}
	} else {
		var why []string
		score, why = shop.Policy{Allowlist: e.cfg.Risk.Allowlist, KnownGateways: e.cfg.Risk.KnownGateways}.Score(info)
		if score < e.cfg.Risk.Threshold {
			return nil, info, score, reject(proto.RejectRisk, "risk score %d below %d (%s)", score, e.cfg.Risk.Threshold, strings.Join(why, ", "))
		}
	}

	// price
	cat, err := e.Shops.Catalog(ctx, req.ShopURL)
	if err != nil {
		e.log.Info("catalog not readable", "order", o.ID, "url", req.ShopURL, "err", err)
		return nil, info, score, reject(proto.RejectUnavailable, "shop catalog not readable")
	}
	cur := cat.Currency
	if !slices.Contains(e.cfg.Currencies, cur) {
		return nil, info, score, reject(proto.RejectUnavailable, "we do not buy in %s", cur)
	}
	items := new(big.Rat)
	for _, it := range req.Items {
		p, ok := cat.Product(it.SKU)
		if !ok || it.Qty <= 0 {
			return nil, info, score, reject(proto.RejectUnavailable, "item %s is not sold", it.SKU)
		}
		price, err := amount.Parse(p.Price.Amount)
		if err != nil || p.Price.Currency != cur {
			return nil, info, score, reject(proto.RejectUnavailable, "price of %s: %v", it.SKU, err)
		}
		items.Add(items, new(big.Rat).Mul(price, big.NewRat(int64(it.Qty), 1)))
	}
	shipping, err := amount.Parse(cat.Shipping.Amount)
	if err != nil {
		return nil, info, score, reject(proto.RejectUnavailable, "shipping: %v", err)
	}
	dec := amount.Decimals(cur)
	sub := new(big.Rat).Add(items, shipping)
	fee := amount.CeilTo(new(big.Rat).Mul(sub, big.NewRat(e.cfg.Fee.BPS, 10000)), dec)
	if e.cfg.Fee.Min.Amount != "" {
		minFee, err := e.convert(ctx, e.cfg.Fee.Min.Amount, e.cfg.Fee.Min.Currency, cur)
		if err != nil {
			return nil, info, score, err
		}
		if minFee = amount.CeilTo(minFee, dec); minFee.Cmp(fee) > 0 {
			fee = minFee
		}
	}
	total := new(big.Rat).Add(sub, fee)
	if e.cfg.MaxOrder.Amount != "" {
		limit, err := e.convert(ctx, e.cfg.MaxOrder.Amount, e.cfg.MaxOrder.Currency, cur)
		if err != nil {
			return nil, info, score, err
		}
		if total.Cmp(limit) > 0 {
			return nil, info, score, reject(proto.RejectLimit, "order of %s %s exceeds our limit of %s %s", amount.FormatCurrency(total, cur), cur, e.cfg.MaxOrder.Amount, e.cfg.MaxOrder.Currency)
		}
	}

	asset := "BTC"
	decimals, reserve := 8, e.cfg.PayoutFeeReserve
	if req.Payment == proto.AssetUSDC {
		asset, decimals, reserve = "USDC", 6, 0
	}
	fq, err := e.FX.Quote(ctx, asset+"/"+cur)
	if err != nil {
		return nil, info, score, reject(proto.RejectUnavailable, "rate %s/%s: %v", asset, cur, err)
	}
	rateStr := fx.FormatRate(fq.Rate)
	rate, _ := fx.ParseRate(rateStr) // compute with the published rate so that users can repeat it
	lock, err := amount.LockAmount(total, rate, decimals, reserve)
	if err != nil {
		return nil, info, score, err
	}
	// user clients refuse a reserve above min(20000 sats, max(2000 sats, 5% of the lock)) (§4.5)
	if reserve > config.MaxPayoutFeeReserve || (reserve > config.MinPayoutFeeReserveCap && big.NewInt(reserve*20).Cmp(lock) > 0) {
		return nil, info, score, reject(proto.RejectLimit, "order too small: the payout fee reserve of %d sats would exceed 5%% of the lock", reserve)
	}
	q := &proto.OrderQuote{
		Accept:    true,
		Detail:    fmt.Sprintf("%s at %s", host, e.Name),
		ExpiresAt: time.Now().Unix() + e.cfg.QuoteTTLSeconds,
		Price: &proto.Price{
			Items:      proto.Money{Amount: amount.FormatCurrency(items, cur), Currency: cur},
			Shipping:   proto.Money{Amount: amount.FormatCurrency(shipping, cur), Currency: cur},
			ShopperFee: proto.Money{Amount: amount.FormatCurrency(fee, cur), Currency: cur},
		},
		FX:               fxOf(fq, rateStr),
		Asset:            req.Payment,
		LockAmount:       lock.String(),
		PayoutFeeReserve: fmt.Sprint(reserve),
	}
	switch req.Payment {
	case proto.AssetBTC:
		err = e.fillBTC(ctx, o, q, escrow, lock)
	case proto.AssetUSDC:
		err = e.fillUSDC(ctx, o, q, escrow, lock)
	}
	if err != nil {
		return nil, info, score, err
	}
	return q, info, score, nil
}

func fxOf(q *fx.Quote, rate string) *proto.FX {
	out := &proto.FX{Pair: q.Pair, Rate: rate, At: q.At}
	for _, s := range q.Sources {
		out.Sources = append(out.Sources, proto.FXSourceRate{Name: s.Name, Rate: fx.FormatRate(s.Rate), At: s.At})
	}
	return out
}

// convert changes an amount from one fiat currency to another through the rates.
func (e *Engine) convert(ctx context.Context, amt, from, to string) (*big.Rat, error) {
	v, err := amount.Parse(amt)
	if err != nil {
		return nil, err
	}
	if from == to || from == "" {
		return v, nil
	}
	q, err := e.FX.Quote(ctx, from+"/"+to)
	if err != nil {
		return nil, reject(proto.RejectUnavailable, "rate %s/%s: %v", from, to, err)
	}
	return v.Mul(v, q.Rate), nil
}

func (e *Engine) fillBTC(ctx context.Context, o *Order, q *proto.OrderQuote, escrow *trust.EscrowProfile, lock *big.Int) error {
	tip, err := e.BTC.TipHeight(ctx)
	if err != nil {
		return reject(proto.RejectUnavailable, "bitcoin tip: %v", err)
	}
	q.Timelock = &proto.Timelock{T1: tip + e.cfg.Timelock.BTCT1Blocks, T2: tip + e.cfg.Timelock.BTCT2Blocks}
	sk, err := e.Keys.OrderKey(o.ID)
	if err != nil {
		return err
	}
	ek, err := keys.EscrowChildPubKey(escrow.BTCXpub, o.ID)
	if err != nil {
		return reject(proto.RejectUnavailable, "escrow xpub: %v", err)
	}
	if _, err := btc.PkScript(escrow.BTCFeeAddress); err != nil {
		return reject(proto.RejectUnavailable, "escrow fee address: %v", err)
	}
	fee := amount.BPS(lock, escrow.UpfrontFee.BPS)
	if escrow.UpfrontFee.MinSats != "" {
		min, err := amount.ParseInt(escrow.UpfrontFee.MinSats)
		if err != nil {
			return reject(proto.RejectUnavailable, "escrow min_sats: %v", err)
		}
		fee = amount.Max(fee, min)
	}
	q.EscrowUpfrontFee = fee.String()
	q.ShopperBTCPubkey = hex.EncodeToString(sk.PubKey().SerializeCompressed())
	q.ShopperBTCAddress = e.Keys.WalletAddress()
	q.EscrowBTCPubkey = hex.EncodeToString(ek.SerializeCompressed())
	q.EscrowBTCFeeAddress = escrow.BTCFeeAddress
	esc, err := contract.BTCEscrow(&o.Request, q)
	if err != nil {
		return err
	}
	q.EscrowAddress, err = esc.Address()
	return err
}

func (e *Engine) fillUSDC(ctx context.Context, o *Order, q *proto.OrderQuote, escrow *trust.EscrowProfile, lock *big.Int) error {
	d, err := e.Deployments()
	if err != nil {
		return reject(proto.RejectUnavailable, "contracts: %v", err)
	}
	now, err := e.EVM.LatestTime(ctx)
	if err != nil {
		return reject(proto.RejectUnavailable, "evm: %v", err)
	}
	if !common.IsHexAddress(escrow.EVMAddress) {
		return reject(proto.RejectUnavailable, "escrow has no EVM address")
	}
	q.Timelock = &proto.Timelock{T1: int64(now) + e.cfg.Timelock.EVMT1Seconds, T2: int64(now) + e.cfg.Timelock.EVMT2Seconds}
	o.ChainExpiresAt = int64(now) + e.cfg.QuoteTTLSeconds
	fee := amount.BPS(lock, escrow.UpfrontFee.BPS)
	if escrow.UpfrontFee.MinUSDC != "" {
		min, err := amount.Parse(escrow.UpfrontFee.MinUSDC)
		if err != nil {
			return reject(proto.RejectUnavailable, "escrow min_usdc: %v", err)
		}
		fee = amount.Max(fee, amount.ToUnits(min, 6))
	}
	q.EscrowUpfrontFee = fee.String()
	q.ShopperEVMAddress = e.Keys.EVMAddress().Hex()
	q.EscrowEVMAddress = common.HexToAddress(escrow.EVMAddress).Hex()
	os, err := contract.SafeOf(d, &o.Request, q)
	if err != nil {
		return err
	}
	safe, err := e.EVM.PredictSafe(ctx, d, os, o.ID)
	if err != nil {
		return reject(proto.RejectUnavailable, "safe address: %v", err)
	}
	q.EscrowAddress = safe.Hex()
	return nil
}

func (e *Engine) onAccept(ctx context.Context, msg *messenger.Message) {
	var acc proto.OrderAccept
	if msg.Decode(&acc) != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		if o.User != msg.From || o.State != StateQuoted {
			return errSkip
		}
		if q := o.Events["quote"]; q == nil || q.ID != acc.QuoteID {
			return errSkip
		}
		if time.Now().Unix() > o.Quote.ExpiresAt {
			o.set(StateCancelled, "quote expired before accept")
			return nil
		}
		o.Events["accept"] = msg.Inner
		o.set(StateAccepted, "")
		return nil
	})
	if err != nil && !errors.Is(err, errSkip) {
		e.log.Warn("accept", "order", msg.OrderID, "err", err)
	}
	if err == nil {
		// an order.funded that overtook the accept was skipped; look at it again
		e.replay(ctx, msg.OrderID, msg.From, proto.TypeOrderFunded, e.onFunded)
	}
}

func (e *Engine) onCancel(ctx context.Context, msg *messenger.Message) {
	var c proto.OrderCancel
	_ = msg.Decode(&c)
	_, _ = e.update(msg.OrderID, func(o *Order) error {
		if o.User != msg.From || (o.State != StateQuoted && o.State != StateAccepted) {
			return errSkip
		}
		o.set(StateCancelled, c.Reason)
		return nil
	})
}

// checkAddress opens the delivery address with key_for_shopper and checks it against the bot schema.
func (e *Engine) checkAddress(o *Order) error {
	addr, err := e.openAddress(o)
	if err != nil {
		return err
	}
	for name, v := range map[string]string{"name": addr.Name, "postal_code": addr.PostalCode, "address": addr.Address, "phone": addr.Phone} {
		if strings.TrimSpace(v) == "" || len(v) > maxAddress {
			return fmt.Errorf("%s must have 1-%d bytes", name, maxAddress)
		}
	}
	return nil
}

func (e *Engine) openAddress(o *Order) (delivery.Address, error) {
	k, err := delivery.UnwrapKey(e.Keys.NostrSecretHex(), o.User, o.Request.Delivery.KeyForShopper)
	if err != nil {
		return delivery.Address{}, err
	}
	return delivery.Open(k, o.Request.Delivery.Ciphertext, o.ID)
}

func isHex32(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}

func short(pk string) string {
	if len(pk) > 12 {
		return pk[:12]
	}
	return pk
}
