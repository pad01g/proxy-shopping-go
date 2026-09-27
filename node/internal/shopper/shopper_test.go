package shopper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/botclient"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

const oid = "000102030405060708090a0b0c0d0e0f"

func labKeys(t *testing.T, name string) *keys.Set {
	t.Helper()
	k, err := keys.LoadMnemonicFile(filepath.Join("..", "..", "..", "lab", "keys", name+".mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// env is a shopper engine (shopper-1) with a trust store naming it with escrow-1 under operator-1, and user-1.
type env struct {
	e                *Engine
	inbox            inboxRec // what the messenger would have stored of the messages handed to the handlers
	chain            *fakeChain
	user, esc        *keys.Set
	opSK, otherOpSK  string
	opPub, otherOp   string
	coordSK, coordPK string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	purchaseRetry = 10 * time.Millisecond
	retryBase, waitPoll = 20*time.Millisecond, 20*time.Millisecond
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	k := labKeys(t, "shopper-1")
	m, _ := messenger.New(messenger.Config{Secret: k.NostrSecretHex(), Pool: nostrnet.NewPool(nil, nil), DB: db})
	st, _ := trust.NewStore(nil)
	v := &env{user: labKeys(t, "user-1"), esc: labKeys(t, "escrow-1"), chain: newFakeChain()}
	v.coordSK, v.opSK, v.otherOpSK = nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	v.coordPK, _ = nostr.GetPublicKey(v.coordSK)
	v.opPub, _ = nostr.GetPublicKey(v.opSK)
	v.otherOp, _ = nostr.GetPublicKey(v.otherOpSK)
	escPub := v.esc.NostrPubHex()
	entry := trust.Entry{Region: "JP-13", Shopper: k.NostrPubHex(), Escrow: escPub, Shops: []string{"safe-shop.test"}, Payments: []string{"btc-signet"}}
	d, _ := trust.NewDelegation(v.coordSK, v.opPub, "ps-lab", 1, false, "")
	l, _ := trust.NewList(v.opSK, 1, &trust.List{Network: "ps-lab", Entries: []trust.Entry{entry},
		Donation: json.RawMessage(`{"btc_address":"` + v.esc.WalletAddress() + `","bps":50}`)})
	// a list by an operator nobody delegated to, with a greedy donation
	l2, _ := trust.NewList(v.otherOpSK, 1, &trust.List{Network: "ps-lab", Entries: []trust.Entry{entry},
		Donation: json.RawMessage(`{"btc_address":"` + v.user.WalletAddress() + `","bps":9000}`)})
	for _, ev := range []*nostr.Event{d, l, l2} {
		if _, err := st.Put(ev); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Shopper{Payments: []string{"btc-signet"}, Currencies: []string{"JPY"}, TrackingPollSeconds: 1,
		Confirmations: 1, PayoutConfirmations: 1, AcceptRulings: "always", MinT1RemainingSeconds: 3600, QuoteTTLSeconds: 900}
	v.e = New(Deps{Keys: k, Messenger: m, Trust: st, Coordinators: []string{v.coordPK}, Network: "ps-lab", DB: db, Config: cfg,
		BTC: v.chain})
	t.Cleanup(v.e.Wait) // before the store closes
	v.e.inbox = v.inbox.get
	// a bot that is down unless a test starts its own
	_, v.e.Bot = startBot(t, func(proto.PurchaseRequest, int) (int, any) { return 502, "down" })
	return v
}

// addEscrowProfile publishes escrow-1's profile (dispute fee 2%).
func (v *env) addEscrowProfile(t *testing.T) {
	t.Helper()
	xpub, _ := v.esc.EscrowXpub()
	ev, err := trust.NewProfile(v.esc.NostrSecretHex(), trust.KindEscrowProfile, "ps-lab", 1, trust.EscrowProfile{
		Name: "escrow-1", BTCXpub: xpub, BTCFeeAddress: v.esc.WalletAddress(), EVMAddress: v.esc.EVMAddress().Hex(),
		UpfrontFee: trust.UpfrontFee{BPS: 50, MinSats: "1000"}, DisputeFeeBPS: 200})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Trust.Put(ev); err != nil {
		t.Fatal(err)
	}
}

// fakeChain is an Esplora stand-in.
type fakeChain struct {
	mu           sync.Mutex
	txs          map[string]*btc.Tx
	conf         map[string]int64
	outspend     map[string]*btc.Outspend
	tip          int64
	txErr        error
	broadcastErr error
	broadcasts   int
	autoConf     int64 // confirmations a broadcast transaction gets at once
}

func newFakeChain() *fakeChain {
	return &fakeChain{txs: map[string]*btc.Tx{}, conf: map[string]int64{}, outspend: map[string]*btc.Outspend{}, tip: 100, autoConf: 1}
}

func (c *fakeChain) TipHeight(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tip, nil
}

func (c *fakeChain) Tx(_ context.Context, txid string) (*btc.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txErr != nil {
		return nil, c.txErr
	}
	if tx := c.txs[txid]; tx != nil {
		return tx, nil
	}
	return nil, &btc.HTTPError{Status: http.StatusNotFound, Body: "not found"}
}

func (c *fakeChain) Confirmations(_ context.Context, txid string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conf[txid], nil
}

func (c *fakeChain) Outspend(_ context.Context, txid string, vout uint32) (*btc.Outspend, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.outspend[fmt.Sprintf("%s:%d", txid, vout)]; s != nil {
		return s, nil
	}
	return &btc.Outspend{}, nil
}

func (c *fakeChain) Broadcast(_ context.Context, txHex string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.broadcasts++
	if c.broadcastErr != nil {
		return "", c.broadcastErr
	}
	raw, _ := hex.DecodeString(txHex)
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return "", err
	}
	id := tx.TxHash().String()
	c.txs[id] = &btc.Tx{TxID: id}
	if c.conf[id] < c.autoConf {
		c.conf[id] = c.autoConf
	}
	return id, nil
}

func (c *fakeChain) broadcastCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broadcasts
}

func (c *fakeChain) set(fn func(c *fakeChain)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// request builds a valid BTC order.request of user-1 for order id.
func (v *env) request(t *testing.T, id string, addr delivery.Address) proto.OrderRequest {
	t.Helper()
	uk, _ := v.user.OrderKey(id)
	k, _ := delivery.NewKey()
	ct, _ := delivery.Seal(k, nil, id, addr)
	kShopper, _ := delivery.WrapKey(v.user.NostrSecretHex(), v.e.Keys.NostrPubHex(), k)
	kEscrow, _ := delivery.WrapKey(v.user.NostrSecretHex(), v.esc.NostrPubHex(), k)
	proof, err := keys.SignKeyProofBTC(uk, id, v.user.NostrPubHex())
	if err != nil {
		t.Fatal(err)
	}
	return proto.OrderRequest{
		ShopURL: "https://safe-shop.test/", ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: "A-100", Qty: 1}},
		Payment: proto.AssetBTC, Escrow: v.esc.NostrPubHex(), Operator: v.opPub, Coordinator: v.coordPK,
		Delivery:      proto.Delivery{Ciphertext: ct, KeyForShopper: kShopper, KeyForEscrowSHA256: proto.EscrowKeyHash(kEscrow)},
		KeyProof:      proof,
		UserBTCPubkey: hex.EncodeToString(uk.PubKey().SerializeCompressed()), UserBTCAddress: v.user.WalletAddress(),
	}
}

var address = delivery.Address{Name: "山田 太郎", PostalCode: "160-0022", Address: "東京都新宿区新宿3-1-1", Phone: "03-0000-0000"}

// fundedOrder stores a quoted BTC order in the given state and puts its funding transaction on the fake chain.
func (v *env) fundedOrder(t *testing.T, id, state string) (*Order, btc.Escrow) {
	t.Helper()
	req := v.request(t, id, address)
	sk, _ := v.e.Keys.OrderKey(id)
	xpub, _ := v.esc.EscrowXpub()
	ek, _ := keys.EscrowChildPubKey(xpub, id)
	q := &proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "100000", EscrowUpfrontFee: "1000", PayoutFeeReserve: "1000",
		ExpiresAt: time.Now().Unix() + 900, Timelock: &proto.Timelock{T1: 1000, T2: 1100},
		Price:            &proto.Price{Items: proto.Money{Amount: "3200", Currency: "JPY"}, Shipping: proto.Money{Amount: "800", Currency: "JPY"}},
		ShopperBTCPubkey: hex.EncodeToString(sk.PubKey().SerializeCompressed()), ShopperBTCAddress: v.e.Keys.WalletAddress(),
		EscrowBTCPubkey: hex.EncodeToString(ek.SerializeCompressed()), EscrowBTCFeeAddress: v.esc.WalletAddress()}
	esc, err := contract.BTCEscrow(&req, q)
	if err != nil {
		t.Fatal(err)
	}
	q.EscrowAddress, _ = esc.Address()
	txid := fmt.Sprintf("%064x", id)
	vout := uint32(0)
	o := &Order{ID: id, User: v.user.NostrPubHex(), Created: time.Now().Unix(), Request: req, Quote: q,
		Events: map[string]*nostr.Event{"request": {}, "quote": {}, "accept": {}}}
	o.set(state, "")
	if state != StateAccepted && state != StateQuoted {
		o.Funded = &proto.OrderFunded{Asset: proto.AssetBTC, TxID: txid, Vout: &vout, Amount: "100000", FeeTxID: txid}
		o.FundingSince = time.Now().Unix()
	}
	if state != StateAccepted && state != StateQuoted && state != StateFunding {
		o.Outpoint = &btc.Outpoint{TxID: txid, Vout: 0, Amount: 100000}
	}
	if err := v.e.db.Put(bucketOrders, id, o); err != nil {
		t.Fatal(err)
	}
	v.chain.set(func(c *fakeChain) {
		c.txs[txid] = fundingTx(t, txid, q.EscrowAddress, 100000, v.esc.WalletAddress(), 1000, time.Now().Unix())
		c.conf[txid] = 1
	})
	return o, esc
}

func fundingTx(t *testing.T, txid, lockAddr string, lock int64, feeAddr string, fee int64, blockTime int64) *btc.Tx {
	t.Helper()
	out := func(addr string, v int64) btc.TxOut {
		pk, err := btc.PkScript(addr)
		if err != nil {
			t.Fatal(err)
		}
		return btc.TxOut{ScriptPubKey: hex.EncodeToString(pk), ScriptPubKeyAddress: addr, Value: v}
	}
	return &btc.Tx{TxID: txid, Vout: []btc.TxOut{out(lockAddr, lock), out(feeAddr, fee)},
		Status: btc.TxStatus{Confirmed: true, BlockHeight: 100, BlockTime: blockTime}}
}

// msg is a message of user-1 (or another sender) to the shopper.
func (v *env) msg(t *testing.T, senderSK, id, typ string, body any) *messenger.Message {
	t.Helper()
	ev, err := giftwrap.NewInner(senderSK, v.e.Keys.NostrPubHex(), id, typ, body, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	v.inbox.add(ev)
	return &messenger.Message{Inner: ev, From: ev.PubKey, Type: typ, OrderID: id}
}

type inboxRec struct {
	mu  sync.Mutex
	evs []*nostr.Event
}

func (r *inboxRec) add(ev *nostr.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *inboxRec) get(orderID string) []*nostr.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*nostr.Event
	for _, ev := range r.evs {
		if giftwrap.OrderID(ev) == orderID {
			out = append(out, ev)
		}
	}
	return out
}

func (v *env) order(t *testing.T, id string) *Order {
	t.Helper()
	o, ok, err := v.e.Order(id)
	if err != nil || !ok {
		t.Fatalf("order %s: %v", id, err)
	}
	return o
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func reason(err error) string {
	var r *rejection
	if errors.As(err, &r) {
		return r.reason
	}
	return "error: " + err.Error()
}

func TestQuoteRejections(t *testing.T) {
	v := newEnv(t)
	base := v.request(t, oid, address)
	mallory := nostr.GeneratePrivateKey()
	malloryPub, _ := nostr.GetPublicKey(mallory)
	cases := []struct {
		name string
		user string
		edit func(r *proto.OrderRequest)
		want string
	}{
		{"payment not offered", "", func(r *proto.OrderRequest) { r.Payment = proto.AssetUSDC }, proto.RejectPayment},
		{"bad user key", "", func(r *proto.OrderRequest) { r.UserBTCPubkey = "02" }, proto.RejectInvalid},
		{"no key proof", "", func(r *proto.OrderRequest) { r.KeyProof = "" }, proto.RejectInvalid},
		// another identity copying the user's chain key cannot prove it holds it (§4.4.1)
		{"key proof of another identity", malloryPub, func(r *proto.OrderRequest) {}, proto.RejectInvalid},
		{"no escrow key commitment", "", func(r *proto.OrderRequest) { r.Delivery.KeyForEscrowSHA256 = "" }, proto.RejectInvalid},
		{"qty 0", "", func(r *proto.OrderRequest) { r.Items = []proto.Item{{SKU: "A-100", Qty: 0}} }, proto.RejectInvalid},
		{"qty 100", "", func(r *proto.OrderRequest) { r.Items = []proto.Item{{SKU: "A-100", Qty: 100}} }, proto.RejectInvalid},
		{"long sku", "", func(r *proto.OrderRequest) { r.Items = []proto.Item{{SKU: strings.Repeat("A", 65), Qty: 1}} }, proto.RejectInvalid},
		{"escrow not listed", "", func(r *proto.OrderRequest) { r.Escrow = v.user.NostrPubHex() }, proto.RejectTrust},
		{"region not covered", "", func(r *proto.OrderRequest) { r.ShopRegion = "JP-27" }, proto.RejectRegion},
		{"shop not listed", "", func(r *proto.OrderRequest) { r.ShopURL = "https://other.test/" }, proto.RejectRegion},
		// the donation comes from the named operator's list: it must be the one listing us
		{"operator not delegated", "", func(r *proto.OrderRequest) { r.Operator = v.otherOp }, proto.RejectTrust},
		{"no escrow profile", "", func(r *proto.OrderRequest) {}, proto.RejectUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			c.edit(&r)
			user := v.user.NostrPubHex()
			if c.user != "" {
				user = c.user
			}
			_, _, _, err := v.e.buildQuote(context.Background(), &Order{ID: oid, User: user, Request: r})
			if err == nil || reason(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
		})
	}

	// with the escrow profile, an address the bot cannot use is refused before the shop is contacted
	v.addEscrowProfile(t)
	r := v.request(t, oid, delivery.Address{Name: "x", PostalCode: "", Address: "y", Phone: "z"})
	if _, _, _, err := v.e.buildQuote(context.Background(), &Order{ID: oid, User: v.user.NostrPubHex(), Request: r}); reason(err) != proto.RejectInvalid {
		t.Fatalf("empty postal code: %v", err)
	}
	r = base
	r.Delivery.Ciphertext = "AAAA"
	if _, _, _, err := v.e.buildQuote(context.Background(), &Order{ID: oid, User: v.user.NostrPubHex(), Request: r}); reason(err) != proto.RejectInvalid {
		t.Fatalf("undecryptable address: %v", err)
	}
}

func TestEscrowKey(t *testing.T) {
	v := newEnv(t)
	v.fundedOrder(t, oid, StateQuoted)
	o := v.order(t, oid)
	// a key that does not match the request's commitment is not kept
	v.e.onEscrowKey(context.Background(), v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: "forged"}))
	if v.order(t, oid).EscrowKey != "" {
		t.Fatal("forged escrow key kept")
	}
	// recompute a matching key: the request commits to key_for_escrow of this ciphertext
	k := "any-key-for-escrow"
	_, _ = v.e.update(oid, func(o *Order) error { o.Request.Delivery.KeyForEscrowSHA256 = proto.EscrowKeyHash(k); return nil })
	v.e.onEscrowKey(context.Background(), v.msg(t, nostr.GeneratePrivateKey(), oid, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: k}))
	if v.order(t, oid).EscrowKey != "" {
		t.Fatal("escrow key of a stranger kept")
	}
	v.e.onEscrowKey(context.Background(), v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: k}))
	got := v.order(t, oid)
	if got.EscrowKey != k || got.Events["escrow_key"] == nil {
		t.Fatalf("escrow key not kept: %+v", got.EscrowKey)
	}
	if ev := v.e.Evidence(got); ev.DeliveryKeyForEscrow != k {
		t.Fatalf("evidence without the escrow key: %+v", ev)
	}
	_ = o
}

func TestFundingNeedsAccept(t *testing.T) {
	v := newEnv(t)
	o, _ := v.fundedOrder(t, oid, StateQuoted)
	vout := uint32(0)
	f := proto.OrderFunded{Asset: proto.AssetBTC, TxID: fmt.Sprintf("%064x", oid), Vout: &vout, Amount: "100000"}
	v.e.onFunded(context.Background(), v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderFunded, f))
	if st := v.order(t, oid).State; st != StateQuoted {
		t.Fatalf("funded without accept moved the order to %s", st)
	}
	_ = o
}

func TestFundingChecks(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.chain.set(func(c *fakeChain) { c.tip = 100 })

	// a funding the chain cannot be asked about (5xx) keeps the order waiting instead of rejecting it
	o, _ := v.fundedOrder(t, "10000000000000000000000000000000", StateFunding)
	v.chain.set(func(c *fakeChain) { c.txErr = &btc.HTTPError{Status: 502, Body: "bad gateway"} })
	v.e.verifyFunding(ctx, o.ID)
	if st := v.order(t, o.ID).State; st != StateFunding {
		t.Fatalf("transient error moved the order to %s", st)
	}
	// not yet indexed (404) also waits
	v.chain.set(func(c *fakeChain) { c.txErr = nil; delete(c.txs, o.Funded.TxID) })
	v.e.verifyFunding(ctx, o.ID)
	if st := v.order(t, o.ID).State; st != StateFunding {
		t.Fatalf("404 moved the order to %s", st)
	}

	// the same outpoint cannot fund two orders
	a, _ := v.fundedOrder(t, "20000000000000000000000000000000", StateFunding)
	b, _ := v.fundedOrder(t, "30000000000000000000000000000000", StateFunding)
	if err := v.e.claimUses(a.ID, []string{"btc:x:0"}); err != nil {
		t.Fatal(err)
	}
	if err := v.e.claimUses(a.ID, []string{"btc:x:0"}); err != nil {
		t.Fatalf("claiming again for the same order: %v", err)
	}
	if err := v.e.claimUses(b.ID, []string{"btc:x:0"}); !contract.IsDefinite(err) {
		t.Fatalf("second order got the same outpoint: %v", err)
	}
	// b names a's funding transaction
	_, _ = v.e.update(b.ID, func(o *Order) error { o.Funded.TxID, o.Funded.FeeTxID = a.Funded.TxID, a.Funded.TxID; return nil })
	_, _ = v.e.update(b.ID, func(o *Order) error { o.Quote = a.Quote; o.Request = a.Request; return nil })
	v.e.verifyFunding(ctx, a.ID)
	if st := v.order(t, a.ID).State; st != StateFunded && st != StatePurchasing && st != StatePurchaseFailed {
		t.Fatalf("a: %s %s", st, v.order(t, a.ID).Error)
	}
	v.e.verifyFunding(ctx, b.ID)
	if got := v.order(t, b.ID); got.State != StateAccepted || !strings.Contains(got.Error, "already funds") {
		t.Fatalf("reused funding: %s %s", got.State, got.Error)
	}

	// the upfront fee must be in the funding transaction itself
	c, _ := v.fundedOrder(t, "40000000000000000000000000000000", StateFunding)
	_, _ = v.e.update(c.ID, func(o *Order) error { o.Funded.FeeTxID = strings.Repeat("ee", 32); return nil })
	v.e.verifyFunding(ctx, c.ID)
	if got := v.order(t, c.ID); got.State != StateAccepted {
		t.Fatalf("fee in another tx: %s", got.State)
	}

	// a funding confirmed after expires_at + 1 h is not bought for; the money is offered back
	d, _ := v.fundedOrder(t, "50000000000000000000000000000000", StateFunding)
	v.chain.set(func(c *fakeChain) { c.txs[d.Funded.TxID].Status.BlockTime = d.Quote.ExpiresAt + 3601 })
	v.e.verifyFunding(ctx, d.ID)
	waitFor(t, "refund of the late funding", func() bool {
		got := v.order(t, d.ID)
		return got.State == StatePurchaseFailed && len(got.Pending) == 0
	})
	if h := fmt.Sprint(v.order(t, d.ID).History); !strings.Contains(h, "funded after the quote expired") {
		t.Fatalf("history %s", h)
	}
}

// bot is a shopper-bot stand-in that counts the purchases per request id.
type bot struct {
	mu     sync.Mutex
	calls  map[string]int
	answer func(req proto.PurchaseRequest, n int) (int, any)
}

func startBot(t *testing.T, answer func(req proto.PurchaseRequest, n int) (int, any)) (*bot, *botclient.Client) {
	b := &bot{calls: map[string]int{}, answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.PurchaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.mu.Lock()
		b.calls[req.RequestID]++
		n := b.calls[req.RequestID]
		b.mu.Unlock()
		code, body := b.answer(req, n)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return b, botclient.New(srv.URL)
}

func (b *bot) count(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}

func TestPurchaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	shot := strings.Repeat("A", 20000) // a screenshot too large for messages
	b, cl := startBot(t, func(req proto.PurchaseRequest, n int) (int, any) {
		if req.RequestID != req.OrderID {
			return 400, map[string]string{"error": "request_id must be the order id"}
		}
		switch req.OrderID[0] {
		case '1': // busy on the first call, then the stored result
			if n == 1 {
				return 409, map[string]string{"error": "in_progress"}
			}
			return 200, proto.PurchaseResult{RequestID: req.RequestID, Status: "ok", ShopOrderID: "S-1", Total: &proto.Money{Amount: "4000", Currency: "JPY"},
				Evidence: []proto.Evidence{{Kind: "screenshot", SHA256: strings.Repeat("0", 64), MIME: "image/png", DataB64: shot}}}
		case '2':
			return 422, map[string]string{"error": "schema"}
		}
		return 502, "down"
	})
	v.e.Bot = cl

	a, _ := v.fundedOrder(t, "10000000000000000000000000000000", StatePurchasing) // e.g. after a restart
	v.e.startPurchase(ctx, a.ID)
	if st := v.order(t, a.ID).State; st != StatePurchasing {
		t.Fatalf("409 moved the order to %s", st)
	}
	v.e.startPurchase(ctx, a.ID)
	got := v.order(t, a.ID)
	if got.State != StatePurchased || b.count(a.ID) != 2 {
		t.Fatalf("state %s after %d calls", got.State, b.count(a.ID))
	}
	// the screenshot is kept apart from the order document
	if got.Purchase.Evidence[0].DataB64 != "" || v.e.PurchaseEvidence(a.ID)[0].DataB64 != shot {
		t.Fatal("purchase evidence stored in the order document")
	}

	// a 4xx is final: the order fails and a refund is offered
	c, _ := v.fundedOrder(t, "20000000000000000000000000000000", StatePurchasing)
	v.e.startPurchase(ctx, c.ID)
	waitFor(t, "purchase failed", func() bool { return v.order(t, c.ID).State == StatePurchaseFailed })

	// network trouble keeps the order purchasing (and the same request id is asked again later)
	d, _ := v.fundedOrder(t, "30000000000000000000000000000000", StatePurchasing)
	v.e.startPurchase(ctx, d.ID)
	if st := v.order(t, d.ID).State; st != StatePurchasing {
		t.Fatalf("5xx moved the order to %s", st)
	}

	// a dispute opened while the bot was buying is not overwritten by the purchase result
	f, _ := v.fundedOrder(t, "11000000000000000000000000000000", StatePurchasing)
	b.mu.Lock()
	b.calls[f.ID] = 1 // skip the 409
	b.mu.Unlock()
	// the dispute arrives while the (slow) bot call is running: start the call on a purchasing order, then dispute
	_, _ = v.e.update(f.ID, func(o *Order) error { o.set(StateDisputed, "wrong item"); return nil })
	v.e.purchaseOnce(ctx, v.order(t, f.ID))
	if got := v.order(t, f.ID); got.State != StateDisputed || got.Purchase == nil {
		t.Fatalf("disputed order: %s %+v", got.State, got.Purchase)
	}
}

func TestNoPurchaseCloseToT1(t *testing.T) {
	v := newEnv(t)
	b, cl := startBot(t, func(req proto.PurchaseRequest, n int) (int, any) { return 200, proto.PurchaseResult{Status: "ok"} })
	v.e.Bot = cl
	o, _ := v.fundedOrder(t, oid, StateFunded)
	v.chain.set(func(c *fakeChain) { c.tip = o.Quote.Timelock.T1 - 5 }) // 3000 s left, 3600 needed
	v.e.startPurchase(context.Background(), oid)
	waitFor(t, "refund offer", func() bool { got := v.order(t, oid); return got.State == StatePurchaseFailed && len(got.Pending) == 0 })
	if b.count(oid) != 0 {
		t.Fatal("bought although T1 is too close")
	}
}

func TestResolveNeedsHuman(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.fundedOrder(t, oid, StateNeedsHuman)
	if _, err := v.e.Resolve(ctx, oid, ResolveRequest{Action: "purchased"}); err == nil {
		t.Fatal("resolved without shop_order_id")
	}
	o, err := v.e.Resolve(ctx, oid, ResolveRequest{Action: "purchased", ShopOrderID: "S-9", Total: &proto.Money{Amount: "4000", Currency: "JPY"}})
	if err != nil || o.State != StatePurchased || o.Purchase.ShopOrderID != "S-9" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := v.e.Resolve(ctx, oid, ResolveRequest{Action: "refund"}); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("resolved twice: %v", err)
	}
	other := "10000000000000000000000000000000"
	v.fundedOrder(t, other, StateNeedsHuman)
	if o, err := v.e.Resolve(ctx, other, ResolveRequest{Action: "refund"}); err != nil || o.State != StatePurchaseFailed {
		t.Fatalf("%+v %v", o, err)
	}
}

// userRelease is the release PSBT of user-1 for an order, paying the shopper lock − reserve (minus donation).
func (v *env) userRelease(t *testing.T, o *Order, esc btc.Escrow, outs []btc.Output) proto.Release {
	t.Helper()
	p, err := esc.NewSpend(*o.Outpoint, outs, btc.PathMultisig)
	if err != nil {
		t.Fatal(err)
	}
	uk, _ := v.user.OrderKey(o.ID)
	if err := btc.Sign(p, uk); err != nil {
		t.Fatal(err)
	}
	b64, _ := btc.EncodePSBT(p)
	return proto.Release{Asset: proto.AssetBTC, PSBT: b64}
}

func TestReleaseIsRetried(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	o, esc := v.fundedOrder(t, oid, StateDelivered)
	rel := v.userRelease(t, o, esc, []btc.Output{{Address: o.Quote.ShopperBTCAddress, Amount: 99000}})
	v.chain.set(func(c *fakeChain) { c.broadcastErr = errors.New("esplora down") })
	// the handler's context ends with the handler; the work it hands off must not end with it
	hctx, cancel := context.WithCancel(ctx)
	v.e.onRelease(hctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeOrderRelease, rel))
	cancel()
	waitFor(t, "a failed broadcast", func() bool { a := v.order(t, oid).Pending[ActRelease]; return a != nil && a.Attempts > 0 })
	if st := v.order(t, oid).State; st != StateDelivered {
		t.Fatalf("state %s", st)
	}
	// the release is not lost: the next round broadcasts it
	v.chain.set(func(c *fakeChain) { c.broadcastErr = nil })
	// (the tick loop does this every 3 s; the first worker may still be finishing, so ask until it runs)
	waitFor(t, "completed", func() bool {
		v.e.retryPending(ctx)
		got := v.order(t, oid)
		return got.State == StateCompleted && len(got.Pending) == 0
	})
	if got := v.order(t, oid); got.PayoutBy != "release" || len(got.Pending) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestDonationOnlyFromTheOrdersOperator(t *testing.T) {
	v := newEnv(t)
	o, esc := v.fundedOrder(t, oid, StateDelivered)
	o.Shop = nil
	addr, _, bps := v.e.donation(o)
	if addr != v.esc.WalletAddress() || bps != 50 {
		t.Fatalf("donation %s %d", addr, bps)
	}
	// the request names an operator that nobody delegated to: its list (bps 9000) is never used
	o.Request.Operator = v.otherOp
	if addr, _, bps := v.e.donation(o); addr != "" || bps != 0 {
		t.Fatalf("donation from an undelegated list: %s %d", addr, bps)
	}
	// a release that pays the greedy donation is refused
	_, _ = v.e.update(oid, func(o *Order) error { o.Request.Operator = v.otherOp; return nil })
	cur := v.order(t, oid)
	rel := v.userRelease(t, cur, esc, []btc.Output{{Address: cur.Quote.ShopperBTCAddress, Amount: 9900}, {Address: v.user.WalletAddress(), Amount: 89100}})
	ev, _ := giftwrap.NewInner(v.user.NostrSecretHex(), v.e.Keys.NostrPubHex(), oid, proto.TypeOrderRelease, rel, nostr.Now())
	if _, err := v.e.prepareRelease(context.Background(), cur, &Action{Event: ev}); !contract.IsDefinite(err) {
		t.Fatalf("greedy donation: %v", err)
	}
	// bps above 1% are capped
	l, _ := trust.NewList(v.opSK, 2, &trust.List{Network: "ps-lab", Entries: []trust.Entry{{Region: "JP-13", Shopper: v.e.Keys.NostrPubHex(),
		Escrow: v.esc.NostrPubHex(), Shops: []string{"safe-shop.test"}, Payments: []string{"btc-signet"}}},
		Donation: json.RawMessage(`{"btc_address":"` + v.esc.WalletAddress() + `","bps":5000}`)})
	if _, err := v.e.Trust.Put(l); err != nil {
		t.Fatal(err)
	}
	o.Request.Operator = v.opPub
	if _, _, bps := v.e.donation(o); bps != maxDonationBPS {
		t.Fatalf("bps %d", bps)
	}
}

// escrowRuling is a ruling of escrow-1 signed with its order key.
func (v *env) escrowRuling(t *testing.T, o *Order, esc btc.Escrow, user, shopper, fee int64) proto.Ruling {
	t.Helper()
	p, err := esc.NewSpend(*o.Outpoint, []btc.Output{
		{Address: o.Request.UserBTCAddress, Amount: user}, {Address: o.Quote.ShopperBTCAddress, Amount: shopper},
		{Address: o.Quote.EscrowBTCFeeAddress, Amount: fee}}, btc.PathMultisig)
	if err != nil {
		t.Fatal(err)
	}
	ek, _ := v.esc.EscrowOrderKey(o.ID)
	if err := btc.Sign(p, ek); err != nil {
		t.Fatal(err)
	}
	b64, _ := btc.EncodePSBT(p)
	return proto.Ruling{Split: proto.Split{User: fmt.Sprint(user), Shopper: fmt.Sprint(shopper), EscrowFee: fmt.Sprint(fee)}, Asset: proto.AssetBTC, PSBT: b64}
}

func TestRulingCountersignRules(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	v.addEscrowProfile(t)
	escSK := v.esc.NostrSecretHex()

	// no open dispute: the ruling is recorded, not countersigned
	a, esc := v.fundedOrder(t, "10000000000000000000000000000000", StateDelivered)
	v.e.onRuling(ctx, v.msg(t, escSK, a.ID, proto.TypeDisputeRuling, v.escrowRuling(t, a, esc, 49000, 49020, 1980)))
	if got := v.order(t, a.ID); got.Ruling == nil || got.Pending[ActRuling] != nil {
		t.Fatalf("ruling without dispute: %+v", got.Pending)
	}
	// the ruled order is no longer claimed after T1
	if v.order(t, a.ID).claimable() {
		t.Fatal("ruled order claimable")
	}

	// an escrow fee above dispute_fee_bps (2% of 99000 = 1980) is refused
	b, esc := v.fundedOrder(t, "20000000000000000000000000000000", StateDisputed)
	_, _ = v.e.update(b.ID, func(o *Order) error { o.Dispute = &Dispute{OpenedBy: o.User}; return nil })
	v.e.onRuling(ctx, v.msg(t, escSK, b.ID, proto.TypeDisputeRuling, v.escrowRuling(t, b, esc, 40000, 49000, 10000)))
	waitFor(t, "greedy ruling refused", func() bool { got := v.order(t, b.ID); return len(got.Pending) == 0 && got.Error != "" })
	if got := v.order(t, b.ID); got.State != StateDisputed || !strings.Contains(got.Error, "escrow fee") || v.chain.broadcastCount() != 0 {
		t.Fatalf("%s %s", got.State, got.Error)
	}

	// a fair ruling of an open dispute is countersigned
	c, esc := v.fundedOrder(t, "30000000000000000000000000000000", StateDisputed)
	_, _ = v.e.update(c.ID, func(o *Order) error { o.Dispute = &Dispute{OpenedBy: o.User}; return nil })
	v.e.onRuling(ctx, v.msg(t, escSK, c.ID, proto.TypeDisputeRuling, v.escrowRuling(t, c, esc, 49000, 48020, 1980)))
	waitFor(t, "settled", func() bool { got := v.order(t, c.ID); return got.State == StateSettled && len(got.Pending) == 0 })
	// a second, different ruling changes nothing
	v.e.onRuling(ctx, v.msg(t, escSK, c.ID, proto.TypeDisputeRuling, v.escrowRuling(t, c, esc, 1000, 96020, 1980)))
	if got := v.order(t, c.ID); got.Ruling.Split.User != "49000" {
		t.Fatalf("ruling replaced: %+v", got.Ruling.Split)
	}
}

func TestCountersignedNeedsTheChain(t *testing.T) {
	ctx := context.Background()
	v := newEnv(t)
	o, _ := v.fundedOrder(t, oid, StateDisputed)
	claim := proto.TxRef{TxID: strings.Repeat("ab", 32)}
	// without a ruling the message is ignored
	v.e.onCountersigned(ctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeDisputeCountersigned, claim))
	if got := v.order(t, oid); got.ClaimedPayout != "" || got.State != StateDisputed {
		t.Fatalf("%+v", got)
	}
	_, _ = v.e.update(oid, func(o *Order) error { o.Ruling = &proto.Ruling{}; return nil })
	v.e.onCountersigned(ctx, v.msg(t, v.user.NostrSecretHex(), oid, proto.TypeDisputeCountersigned, claim))
	v.e.watchEscrows(ctx)
	v.e.Wait()
	if got := v.order(t, oid); got.State != StateDisputed || got.PayoutTx != "" || !got.funded() {
		t.Fatalf("a message alone settled the order: %s", got.State)
	}
	// once the chain shows the escrow spent by that transaction, the order is settled
	v.chain.set(func(c *fakeChain) {
		c.outspend[fmt.Sprintf("%s:%d", o.Outpoint.TxID, o.Outpoint.Vout)] = &btc.Outspend{Spent: true, TxID: claim.TxID}
	})
	v.e.watchEscrows(ctx)
	v.e.Wait()
	if got := v.order(t, oid); got.State != StateSettled || got.PayoutTx != claim.TxID {
		t.Fatalf("%s %s", got.State, got.PayoutTx)
	}
}

func TestHistoryDoesNotGrowWithRetries(t *testing.T) {
	o := &Order{}
	for i := 0; i < 100; i++ {
		o.note("purchase call failed: connection refused")
	}
	o.note("other")
	if len(o.History) != 2 {
		t.Fatalf("%d history lines", len(o.History))
	}
}

func TestOrderHelpers(t *testing.T) {
	m, err := sumMoney(proto.Money{Amount: "3200", Currency: "JPY"}, proto.Money{Amount: "800", Currency: "JPY"})
	if err != nil || m.Amount != "4000" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := sumMoney(proto.Money{Amount: "1", Currency: "JPY"}, proto.Money{Amount: "1", Currency: "USD"}); err == nil {
		t.Fatal("mixed currencies summed")
	}
	o := &Order{State: StateDelivered, ShipStatus: "delivered", Outpoint: nil, Safe: "0x1"}
	if !o.funded() || !o.claimable() {
		t.Fatal("delivered funded order must be claimable")
	}
	o.PayoutTx = "tx"
	if o.funded() || o.claimable() {
		t.Fatal("paid out order still open")
	}
	for _, s := range []string{StateCompleted, StateSettled, StateClaimed, StateClosed, StateRejected, StateCancelled} {
		if !terminal(s) {
			t.Errorf("%s not terminal", s)
		}
	}
	if terminal(StateDisputed) {
		t.Error("disputed is not terminal")
	}
}
