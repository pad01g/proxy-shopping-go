package shopper

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// A ruling the shopper did not countersign (no dispute known, or declined) does not keep it from claiming
// through T1 while the escrow is unspent (review item 3).
func TestDeclinedRulingDoesNotBlockClaim(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.addEscrowProfile(t)
	o, esc := v.fundedOrder(t, oid, StateDelivered)
	_, _ = v.e.update(oid, func(o *Order) error { o.ShipStatus = "delivered"; return nil })
	// the escrow rules for the user (the shopper gets nothing, so it would not countersign anyway)
	v.e.onRuling(ctx, v.msg(t, v.esc.NostrSecretHex(), oid, proto.TypeDisputeRuling, v.escrowRuling(t, o, esc, 98010, 0, 990)))
	got := v.order(t, oid)
	if got.Ruling == nil || got.Pending[ActRuling] != nil || !got.claimable() {
		t.Fatalf("ruling pending %v, claimable %v", got.Pending, got.claimable())
	}
	// at T1 the claim goes out
	v.chain.set(func(c *fakeChain) { c.tip = o.Quote.Timelock.T1 })
	v.e.watchEscrows(ctx)
	waitFor(t, "claimed", func() bool { v.e.retryPending(ctx); return v.order(t, oid).State == StateClaimed })
}

// A ruling that arrives before the dispute is known is kept and countersigned once the escrow's evidence
// request (or the dispute.open copy) shows the dispute (§4.8).
func TestRulingKeptUntilTheDisputeIsKnown(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.addEscrowProfile(t)
	escSK := v.esc.NostrSecretHex()
	for i, via := range []string{proto.TypeDisputeEvidenceRequest, proto.TypeDisputeOpen} {
		id := fmt.Sprintf("%d0000000000000000000000000000000", i+1)
		o, esc := v.fundedOrder(t, id, StateDelivered)
		v.e.onRuling(ctx, v.msg(t, escSK, id, proto.TypeDisputeRuling, v.escrowRuling(t, o, esc, 49000, 48020, 1980)))
		if got := v.order(t, id); got.Pending[ActRuling] != nil || got.RulingDecided {
			t.Fatalf("%s: countersigned before the dispute was known", via)
		}
		switch via {
		case proto.TypeDisputeEvidenceRequest:
			v.e.onEvidenceRequest(ctx, v.msg(t, escSK, id, via, proto.EvidenceRequest{Want: []string{"messages"}}))
		case proto.TypeDisputeOpen:
			v.e.onDisputeOpen(ctx, v.msg(t, v.user.NostrSecretHex(), id, via, proto.DisputeOpen{Claim: proto.ClaimNotDelivered}))
		}
		waitFor(t, via+": settled", func() bool {
			v.e.retryPending(ctx)
			got := v.order(t, id)
			return got.State == StateSettled && len(got.Pending) == 0
		})
		// a later dispute.open does not take the order back
		v.e.onDisputeOpen(ctx, v.msg(t, v.user.NostrSecretHex(), id, proto.TypeDisputeOpen, proto.DisputeOpen{Claim: proto.ClaimOther}))
		if got := v.order(t, id); got.State != StateSettled {
			t.Fatalf("%s: %s after a late dispute.open", via, got.State)
		}
	}
}

// An order.accept that arrives while the quote is on its way is not lost, and a quote an earlier attempt sent
// (before a crash) is not followed by a second one (review item 4).
func TestQuoteStoredBeforeItLeaves(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)

	// crashed after the quote left, before anything was stored
	o, _ := v.fundedOrder(t, oid, StateRequested)
	q := *o.Quote
	_, _ = v.e.update(oid, func(o *Order) error {
		o.Quote, o.Events = nil, map[string]*nostr.Event{"request": o.Events["request"]}
		o.addPending(ActQuote, &Action{})
		return nil
	})
	sent, err := v.e.Messenger.Send(ctx, v.user.NostrPubHex(), oid, proto.TypeOrderQuote, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	// the user accepted it at once; the accept came before the quote was recorded
	v.e.onAccept(ctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: sent.ID}))
	v.e.kick(ctx, oid)
	waitFor(t, "accepted", func() bool { return v.order(t, oid).State == StateAccepted })
	got := v.order(t, oid)
	if got.Events["quote"].ID != sent.ID || got.Events["accept"] == nil || len(got.Pending) != 0 {
		t.Fatalf("quote %v accept %v pending %v", got.Events["quote"].ID, got.Events["accept"], got.Pending)
	}
	quotes := 0
	for _, ev := range v.e.Messenger.Outbox(oid) {
		if giftwrap.Type(ev) == proto.TypeOrderQuote {
			quotes++
		}
	}
	if quotes != 1 {
		t.Fatalf("%d quotes sent", quotes)
	}

	// stored as quoted before sending (the send failed): the stored quote is what goes out, and an accept of it
	// that came early is taken
	other := "10000000000000000000000000000000"
	o2, _ := v.fundedOrder(t, other, StateQuoted)
	_, _ = v.e.update(other, func(o *Order) error {
		o.Events = map[string]*nostr.Event{"request": o.Events["request"]}
		o.addPending(ActQuote, &Action{})
		return nil
	})
	if err := v.e.doQuote(ctx, v.order(t, other)); err != nil {
		t.Fatal(err)
	}
	got = v.order(t, other)
	if got.State != StateQuoted || got.Events["quote"] == nil || got.Quote.LockAmount != o2.Quote.LockAmount {
		t.Fatalf("%s %+v", got.State, got.Events["quote"])
	}
}

// Pending actions are paced and not dropped after a fixed number of attempts; one that sent a transaction is
// never dropped while the chain has not decided (review item 6).
func TestPendingActionsBackOff(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	o, _ := v.fundedOrder(t, oid, StateDelivered)
	_, _ = v.e.update(oid, func(o *Order) error { o.addPending(ActRelease, &Action{}); return nil })
	transient := errors.New("esplora down")
	for i := 0; i < 80; i++ {
		v.e.settle(ctx, o, ActRelease, transient)
	}
	a := v.order(t, oid).Pending[ActRelease]
	if a == nil || a.Attempts != 80 || a.Next <= time.Now().Add(retryMax/2).Unix() {
		t.Fatalf("action %+v", a)
	}
	// waiting for a sent transaction is not an attempt
	v.e.settle(ctx, o, ActRelease, errWaiting)
	if a := v.order(t, oid).Pending[ActRelease]; a.Attempts != 80 {
		t.Fatalf("waiting counted: %d", a.Attempts)
	}
	// with a transaction sent, even an old action with a definite error stays
	_, _ = v.e.update(oid, func(o *Order) error {
		o.Pending[ActRelease].Tx, o.Pending[ActRelease].Since = strings.Repeat("ab", 32), time.Now().Add(-2*actionTTL).Unix()
		return nil
	})
	v.e.settle(ctx, o, ActRelease, transient)
	if v.order(t, oid).Pending[ActRelease] == nil {
		t.Fatal("an action with a sent transaction was dropped")
	}
	// without one, an action older than actionTTL is given up
	_, _ = v.e.update(oid, func(o *Order) error { o.Pending[ActRelease].Tx = ""; return nil })
	v.e.settle(ctx, o, ActRelease, transient)
	if v.order(t, oid).Pending[ActRelease] != nil {
		t.Fatal("expired action kept")
	}
}

// A BTC payout is final only with its confirmations; one that drops out of the mempool is broadcast again
// (review item 10).
func TestPayoutWatchedUntilConfirmed(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.e.cfg.PayoutConfirmations = 2
	v.chain.set(func(c *fakeChain) { c.autoConf = 0 })
	o, esc := v.fundedOrder(t, oid, StateDelivered)
	rel := v.userRelease(t, o, esc, []btc.Output{{Address: o.Quote.ShopperBTCAddress, Amount: 99000}})
	v.e.onRelease(ctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderRelease, rel))
	waitFor(t, "completed", func() bool { return v.order(t, oid).State == StateCompleted })
	v.e.Wait()
	a := v.order(t, oid).Pending[ActRelease]
	if a == nil || a.Tx == "" || a.Raw == "" {
		t.Fatalf("payout not watched: %+v", a)
	}
	// the transaction drops out: it is broadcast again as it was
	n := v.chain.broadcastCount()
	v.chain.set(func(c *fakeChain) { delete(c.txs, a.Tx) })
	waitFor(t, "broadcast again", func() bool { v.e.retryPending(ctx); v.e.Wait(); return v.chain.broadcastCount() > n })
	if got := v.order(t, oid).Pending[ActRelease]; got == nil || got.Tx != a.Tx {
		t.Fatalf("%+v", got)
	}
	// with its confirmations it is final
	v.chain.set(func(c *fakeChain) { c.conf[a.Tx] = 2 })
	waitFor(t, "final", func() bool { v.e.retryPending(ctx); return len(v.order(t, oid).Pending) == 0 })
	if got := v.order(t, oid); got.PayoutTx != a.Tx || got.State != StateCompleted {
		t.Fatalf("%s %s", got.State, got.PayoutTx)
	}
}

// Accepted quotes nobody funds in time are cancelled; a funding that still comes is given back, not bought for
// (review item 8).
func TestUnfundedQuoteExpires(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	o, _ := v.fundedOrder(t, oid, StateAccepted)
	_, _ = v.e.update(oid, func(o *Order) error { o.Quote.ExpiresAt = time.Now().Unix() - fundingGrace - 1; return nil })
	v.e.checkFunding(ctx)
	if got := v.order(t, oid); got.State != StateCancelled || !got.FundingExpired {
		t.Fatalf("%s", got.State)
	}
	vout := uint32(0)
	txid := fmt.Sprintf("%064x", oid)
	v.e.onFunded(ctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: txid, Vout: &vout, FeeTxID: txid, Amount: "100000"}))
	waitFor(t, "refund offered", func() bool {
		got := v.order(t, oid)
		return got.State == StatePurchaseFailed && len(got.Pending) == 0
	})
	_ = o
	// a funded message with an id in capitals is not taken
	other := "10000000000000000000000000000000"
	v.fundedOrder(t, other, StateAccepted)
	v.e.onFunded(ctx, v.msg(t, v.user.NostrSecretHex(), other, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: strings.ToUpper(strings.Repeat("ab", 32)), Vout: &vout, Amount: "100000"}))
	if st := v.order(t, other).State; st != StateAccepted {
		t.Fatalf("upper-case txid taken: %s", st)
	}
}

// A rejected request is answered once: its messages are not resent (review item 9).
func TestWantsRetry(t *testing.T) {
	v := newEnv(t)
	v.fundedOrder(t, oid, StateRejected)
	other := "10000000000000000000000000000000"
	v.fundedOrder(t, other, StateQuoted)
	if v.e.WantsRetry(oid) || !v.e.WantsRetry(other) || v.e.WantsRetry("20000000000000000000000000000000") {
		t.Fatal("WantsRetry")
	}
}

// order.purchased always carries a total (review item 11).
func TestPurchasedNeedsATotal(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	_, cl := startBot(t, func(req proto.PurchaseRequest, n int) (int, any) {
		return 200, proto.PurchaseResult{RequestID: req.RequestID, Status: "ok", ShopOrderID: "S-1"}
	})
	v.e.Bot = cl
	v.fundedOrder(t, oid, StatePurchasing)
	v.e.startPurchase(ctx, oid)
	if got := v.order(t, oid); got.State != StateNeedsHuman {
		t.Fatalf("purchased without a total: %s", got.State)
	}
	for _, total := range []*proto.Money{nil, {Amount: "", Currency: "JPY"}, {Amount: "-1", Currency: "JPY"}, {Amount: "4000"}} {
		if _, err := v.e.Resolve(ctx, oid, ResolveRequest{Action: "purchased", ShopOrderID: "S-1", Total: total}); err == nil {
			t.Fatalf("resolved with total %+v", total)
		}
	}
	if o, err := v.e.Resolve(ctx, oid, ResolveRequest{Action: "purchased", ShopOrderID: "S-1", Total: &proto.Money{Amount: "4000", Currency: "JPY"}}); err != nil || o.State != StatePurchased {
		t.Fatalf("%v", err)
	}
}

// Tracking evidence goes into messages only as far as it fits; the rest goes as attachments within the
// escrow's limits, and the first dispute.evidence fits one message (review item 11, 13).
func TestEvidenceFitsTheLimits(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.fundedOrder(t, oid, StatePurchased)
	item := func(n int, c string) proto.Evidence {
		return proto.Evidence{Kind: "screenshot", SHA256: strings.Repeat(c, 64), MIME: "image/png", DataB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat(c, n)))}
	}
	big := item(100<<10, "a")                                        // 9 chunks
	huge := item((maxAttachmentChunks+1)*proto.AttachmentChunk, "b") // beyond the chunk limit
	small := []proto.Evidence{item(7<<10, "c"), item(7<<10, "d"), item(7<<10, "e"), item(7<<10, "f")}
	st := &proto.TrackingStatus{Status: "shipped", Evidence: append([]proto.Evidence{big, huge}, small...)}
	v.e.reportShipping(ctx, oid, st)
	o := v.order(t, oid)
	for _, ev := range o.Tracking[0].Evidence {
		if base64.StdEncoding.DecodedLen(len(ev.DataB64)) > proto.InlineEvidenceMax {
			t.Fatal("large tracking evidence stored in the order")
		}
	}
	if n := len(v.e.TrackingEvidence(oid)); n != 6 {
		t.Fatalf("%d tracking items kept", n)
	}
	ev, attach, skipped := v.e.evidenceParts(o)
	if firstSize(ev) > evidenceBudget {
		t.Fatalf("first part %d bytes", firstSize(ev))
	}
	if len(attach) > maxAttachments || skipped == 0 {
		t.Fatalf("%d attachments, %d skipped", len(attach), skipped)
	}
	for _, a := range attach {
		if a.SHA256 == huge.SHA256 {
			t.Fatal("item beyond the chunk limit sent")
		}
	}
	if parts := splitEvidence(ev); len(parts) == 0 {
		t.Fatal("no parts")
	}
}

// The watch loop does not wait for the lock of an order that is busy (review item 13).
func TestWatchDoesNotWaitForBusyOrders(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	a, _ := v.fundedOrder(t, "10000000000000000000000000000000", StateDelivered)
	b, _ := v.fundedOrder(t, "20000000000000000000000000000000", StateDelivered)
	v.chain.set(func(c *fakeChain) {
		for _, o := range []*Order{a, b} {
			c.outspend[fmt.Sprintf("%s:0", o.Outpoint.TxID)] = &btc.Outspend{Spent: true, TxID: strings.Repeat("99", 32)}
		}
	})
	unlock := v.e.lock(a.ID) // e.g. a payout of a waiting for its receipt
	done := make(chan struct{})
	go func() { v.e.watchEscrows(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the watch loop waits for a busy order")
	}
	waitFor(t, "b closed", func() bool { return v.order(t, b.ID).State == StateClosed })
	unlock()
	waitFor(t, "a closed", func() bool { return v.order(t, a.ID).State == StateClosed })
}

// The donation is the one of the list at quote time (review item 13).
func TestDonationFixedAtQuote(t *testing.T) {
	v := newEnv(t)
	o, esc := v.fundedOrder(t, oid, StateDelivered)
	_, _ = v.e.update(oid, func(o *Order) error { o.Donation = &Donation{BPS: 0}; return nil })
	cur := v.order(t, oid)
	// the list now asks for a donation, the order was quoted without one: a release paying the shopper all is good
	rel := v.userRelease(t, cur, esc, []btc.Output{{Address: cur.Quote.ShopperBTCAddress, Amount: 99000}})
	ev, _ := giftwrap.NewInner(v.user.NostrSecretHex(), v.e.Keys.NostrPubHex(), oid, proto.TypeOrderRelease, rel, nostr.Now())
	if _, err := v.e.prepareRelease(context.Background(), cur, &Action{Event: ev}); err != nil {
		t.Fatalf("release under the quoted donation: %v", err)
	}
	_ = o
	_ = psbt.Packet{}
}
