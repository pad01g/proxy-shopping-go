package escrow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

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

func testEngine(t *testing.T) *Engine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	k := labKeys(t, "escrow-1")
	m, _ := messenger.New(messenger.Config{Secret: k.NostrSecretHex(), Pool: nostrnet.NewPool(nil, nil), DB: db})
	e := New(Deps{Keys: k, Messenger: m, DB: db, Config: &config.Escrow{
		UpfrontFee: config.UpfrontFee{BPS: 50, MinSats: "1000", MinUSDC: "0.50"}, DisputeFeeBPS: 200}, Name: "escrow-1"})
	t.Cleanup(e.Wait)
	// the messages the tests hand to the handlers are what the messenger would have stored
	testInbox = &inboxRec{}
	e.inbox = testInbox.get
	return e
}

// inboxRec stands in for the messenger's store of received messages.
type inboxRec struct {
	mu  sync.Mutex
	evs []*nostr.Event
}

var testInbox = &inboxRec{}

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

// chain is an Esplora stand-in.
type chain struct {
	mu       sync.Mutex
	txs      map[string]*btc.Tx
	err      error
	outspend map[string]*btc.Outspend
}

func (c *chain) Tx(_ context.Context, txid string) (*btc.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if tx := c.txs[txid]; tx != nil {
		return tx, nil
	}
	return nil, &btc.HTTPError{Status: http.StatusNotFound}
}

func (c *chain) Confirmations(context.Context, string) (int64, error) { return 1, nil }

func (c *chain) Outspend(_ context.Context, txid string, vout uint32) (*btc.Outspend, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.outspend[fmt.Sprintf("%s:%d", txid, vout)]; s != nil {
		return s, nil
	}
	return &btc.Outspend{}, nil
}

func (c *chain) set(fn func(c *chain)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// order is a signed BTC agreement between user-1 and shopper-1 with escrow-1.
type order struct {
	id                            string
	user, shopper                 *keys.Set
	request, quote, accept, funds *nostr.Event
	keyForEscrow                  string
	lockAddr                      string
	txid                          string
}

func newOrder(t *testing.T, e *Engine, c *chain, id string, fundTxID string) *order {
	t.Helper()
	return newOrderQ(t, e, c, id, fundTxID, nil)
}

// newOrderQ is newOrder with a quote changed by edit (before the escrow address is computed).
func newOrderQ(t *testing.T, e *Engine, c *chain, id string, fundTxID string, edit func(q *proto.OrderQuote)) *order {
	t.Helper()
	o := &order{id: id, user: labKeys(t, "user-1"), shopper: labKeys(t, "shopper-1"), txid: fundTxID}
	k, _ := delivery.NewKey()
	ct, _ := delivery.Seal(k, nil, id, delivery.Address{Name: "山田 太郎", PostalCode: "160-0022", Address: "東京都新宿区", Phone: "03"})
	o.keyForEscrow, _ = delivery.WrapKey(o.user.NostrSecretHex(), e.Keys.NostrPubHex(), k)
	uk, _ := o.user.OrderKey(id)
	proof, _ := keys.SignKeyProofBTC(uk, id, o.user.NostrPubHex())
	req := proto.OrderRequest{Payment: proto.AssetBTC, Escrow: e.Keys.NostrPubHex(), KeyProof: proof,
		Delivery:      proto.Delivery{Ciphertext: ct, KeyForShopper: "k", KeyForEscrowSHA256: proto.EscrowKeyHash(o.keyForEscrow)},
		UserBTCPubkey: hex.EncodeToString(uk.PubKey().SerializeCompressed()), UserBTCAddress: o.user.WalletAddress()}
	sk, _ := o.shopper.OrderKey(id)
	ek, _ := e.Keys.EscrowOrderKey(id)
	q := proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "100000", EscrowUpfrontFee: "1000", PayoutFeeReserve: "1000",
		Timelock: &proto.Timelock{T1: 1000, T2: 1100}, ShopperBTCPubkey: hex.EncodeToString(sk.PubKey().SerializeCompressed()),
		ShopperBTCAddress: o.shopper.WalletAddress(), EscrowBTCPubkey: hex.EncodeToString(ek.PubKey().SerializeCompressed()),
		EscrowBTCFeeAddress: e.Keys.WalletAddress()}
	if edit != nil {
		edit(&q)
	}
	esc, err := contract.BTCEscrow(&req, &q)
	if err != nil {
		t.Fatal(err)
	}
	q.EscrowAddress, _ = esc.Address()
	o.lockAddr = q.EscrowAddress
	o.request = inner(t, o.user, o.shopper.NostrPubHex(), id, proto.TypeOrderRequest, req)
	o.quote = inner(t, o.shopper, o.user.NostrPubHex(), id, proto.TypeOrderQuote, q)
	o.accept = inner(t, o.user, o.shopper.NostrPubHex(), id, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: o.quote.ID})
	vout := uint32(0)
	o.funds = inner(t, o.user, o.shopper.NostrPubHex(), id, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: fundTxID, Vout: &vout, FeeTxID: fundTxID, Amount: "100000"})
	c.set(func(c *chain) {
		c.txs[fundTxID] = &btc.Tx{TxID: fundTxID, Vout: []btc.TxOut{out(t, o.lockAddr, 100000), out(t, e.Keys.WalletAddress(), 1000)}, Status: btc.TxStatus{Confirmed: true}}
	})
	return o
}

func out(t *testing.T, addr string, v int64) btc.TxOut {
	pk, err := btc.PkScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	return btc.TxOut{ScriptPubKey: hex.EncodeToString(pk), ScriptPubKeyAddress: addr, Value: v}
}

func inner(t *testing.T, from *keys.Set, to, id, typ string, body any) *nostr.Event {
	t.Helper()
	ev, err := giftwrap.NewInner(from.NostrSecretHex(), to, id, typ, body, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func message(t *testing.T, fromSK, to, id, typ string, body any) *messenger.Message {
	t.Helper()
	ev, err := giftwrap.NewInner(fromSK, to, id, typ, body, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	testInbox.add(ev)
	return &messenger.Message{Inner: ev, From: ev.PubKey, Type: typ, OrderID: id}
}

func (o *order) events() []*nostr.Event { return []*nostr.Event{o.request, o.quote, o.accept, o.funds} }

func (o *order) notice(t *testing.T, e *Engine) {
	t.Helper()
	e.onNotice(context.Background(), message(t, o.user.NostrSecretHex(), e.Keys.NostrPubHex(), o.id, proto.TypeEscrowNotice,
		proto.EscrowNotice{Request: o.request, Quote: o.quote, Accept: o.accept, Funded: o.funds}))
}

func (o *order) open(t *testing.T, e *Engine, ev proto.DisputeEvidence) {
	t.Helper()
	e.onDisputeOpen(context.Background(), message(t, o.user.NostrSecretHex(), e.Keys.NostrPubHex(), o.id, proto.TypeDisputeOpen,
		proto.DisputeOpen{Claim: proto.ClaimNotDelivered, Evidence: ev}))
	e.Wait()
}

func setup(t *testing.T) (*Engine, *chain) {
	e := testEngine(t)
	c := &chain{txs: map[string]*btc.Tx{}, outspend: map[string]*btc.Outspend{}}
	e.BTC = c
	return e, c
}

func mustCase(t *testing.T, e *Engine, id string) *Case {
	t.Helper()
	c, ok, err := e.Case(id)
	if err != nil || !ok {
		t.Fatalf("no case %s: %v", id, err)
	}
	return c
}

func TestSplit(t *testing.T) {
	e := testEngine(t)
	fee, err := e.split(big.NewInt(89600), big.NewInt(40000), big.NewInt(47808), btc.DustLimit)
	if err != nil || fee.Int64() != 1792 {
		t.Fatalf("fee %v %v", fee, err)
	}
	if _, err := e.split(big.NewInt(89600), big.NewInt(40000), big.NewInt(49600), btc.DustLimit); err == nil {
		t.Fatal("split ignoring the dispute fee accepted")
	}
	// a fee below dust is not taken (2% of 20000 = 400 sats would be an output that does not relay)
	if fee, err := e.split(big.NewInt(20000), big.NewInt(10000), big.NewInt(10000), btc.DustLimit); err != nil || fee.Sign() != 0 {
		t.Fatalf("dust fee %v %v", fee, err)
	}
	if fee, err := e.split(big.NewInt(20000), big.NewInt(10000), big.NewInt(9600), 0); err != nil || fee.Int64() != 400 {
		t.Fatalf("usdc fee %v %v", fee, err)
	}
}

func TestProfileAndNoObligation(t *testing.T) {
	e := testEngine(t)
	p, err := e.Profile(nil)
	if err != nil || p.BTCXpub == "" || p.BTCFeeAddress != e.Keys.WalletAddress() || p.DisputeFeeBPS != 200 || p.UpfrontFee.MinSats != "1000" {
		t.Fatalf("profile %+v %v", p, err)
	}
	_ = e.DB.Put(bucketCases, oid, &Case{OrderID: oid, State: CaseNoObligation})
	_, err = e.Rule(context.Background(), oid, RuleRequest{User: "1", Shopper: "1"})
	if !errors.Is(err, ErrNoObligation) {
		t.Fatalf("rule on an unpaid case: %v", err)
	}
	if _, err := e.Rule(context.Background(), "ffffffffffffffffffffffffffffffff", RuleRequest{}); err == nil {
		t.Fatal("rule without a case")
	}
}

func TestCaseCannotBeTakenOver(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)

	// mallory writes her own request for the same order (copying the user's chain key) and a quote she forged
	// with her own "shopper" key; with the notice recorded, the case follows the notice and she is nobody
	mallory := nostr.GeneratePrivateKey()
	var req proto.OrderRequest
	_ = json.Unmarshal([]byte(o.request.Content), &req)
	mreq, _ := giftwrap.NewInner(mallory, o.shopper.NostrPubHex(), oid, proto.TypeOrderRequest, req, nostr.Now())
	e.onDisputeOpen(context.Background(), message(t, mallory, e.Keys.NostrPubHex(), oid, proto.TypeDisputeOpen,
		proto.DisputeOpen{Claim: proto.ClaimNotDelivered, Evidence: proto.DisputeEvidence{Messages: []*nostr.Event{mreq, o.quote}}}))
	e.Wait()
	if _, ok, _ := e.Case(oid); ok {
		t.Fatal("a stranger opened the case")
	}

	// without a notice, her request does not verify (key_proof), and two requests for one order are refused
	other := "10000000000000000000000000000000"
	o2 := newOrder(t, e, c, other, strings.Repeat("bb", 32))
	mreq2, _ := giftwrap.NewInner(mallory, o2.shopper.NostrPubHex(), other, proto.TypeOrderRequest, req, nostr.Now())
	mq := inner(t, o2.shopper, mreq2.PubKey, other, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC})
	for _, evs := range [][]*nostr.Event{{mreq2, mq}, append(o2.events(), mreq2)} {
		e.onDisputeOpen(context.Background(), message(t, mallory, e.Keys.NostrPubHex(), other, proto.TypeDisputeOpen,
			proto.DisputeOpen{Claim: proto.ClaimNotDelivered, Evidence: proto.DisputeEvidence{Messages: evs}}))
		e.Wait()
		if _, ok, _ := e.Case(other); ok {
			t.Fatal("case opened from a forged request")
		}
	}

	// the user opens the case; mallory's differing messages in the evidence are ignored
	o.open(t, e, proto.DisputeEvidence{Messages: []*nostr.Event{mreq, o.quote}})
	cs := mustCase(t, e, oid)
	if cs.User != o.user.NostrPubHex() || cs.State != CaseOpen || !cs.FeePaid || !cs.Verified || !strings.Contains(strings.Join(cs.History, "\n"), "differs from the verified order") {
		t.Fatalf("%+v", cs)
	}
	if n := e.notices(oid); len(n) != 1 || n[0].requestID() != o.request.ID {
		t.Fatalf("notices %+v", n)
	}
}

func TestFeeCheck(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	// Esplora failing: the case waits for the fee check instead of refusing forever
	c.set(func(c *chain) { c.err = &btc.HTTPError{Status: 502} })
	o.open(t, e, proto.DisputeEvidence{})
	if cs := mustCase(t, e, oid); cs.State != CaseFeePending {
		t.Fatalf("state %s", cs.State)
	}
	if _, err := e.Rule(context.Background(), oid, RuleRequest{User: "1", Shopper: "1"}); err == nil || errors.Is(err, ErrNoObligation) {
		t.Fatalf("rule while the fee is unchecked: %v", err)
	}
	c.set(func(c *chain) { c.err = nil })
	// the next check is paced; once it is due the case opens
	if cs := mustCase(t, e, oid); cs.NextCheck <= time.Now().Unix() {
		t.Fatalf("no backoff: %d", cs.NextCheck)
	}
	_ = store.Modify(e.DB, bucketCases, oid, func(c *Case, _ bool) error { c.NextCheck = 0; return nil })
	e.watch(context.Background())
	e.Wait()
	if cs := mustCase(t, e, oid); cs.State != CaseOpen || !cs.FeePaid {
		t.Fatalf("state %s", cs.State)
	}

	// a second order whose escrow output sits in the same transaction (so the same fee): the fee counts for the
	// first order; the conflict is checked again (not final) until the case expires
	other := "10000000000000000000000000000000"
	o2 := newOrder(t, e, c, other, strings.Repeat("bb", 32))
	c.set(func(c *chain) {
		tx := c.txs[strings.Repeat("aa", 32)]
		o2.funds = inner(t, o2.user, o2.shopper.NostrPubHex(), other, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC,
			TxID: tx.TxID, Vout: ptr(uint32(2)), FeeTxID: tx.TxID, Amount: "100000"})
		tx.Vout = append(tx.Vout, out(t, o2.lockAddr, 100000))
	})
	o2.notice(t, e)
	o2.open(t, e, proto.DisputeEvidence{})
	if cs := mustCase(t, e, other); cs.State != CaseFeePending || !strings.Contains(strings.Join(cs.History, "\n"), "already paid for order") || cs.NextCheck == 0 {
		t.Fatalf("reused fee: %s %v", cs.State, cs.History)
	}
	PendingTTL = 0
	t.Cleanup(func() { PendingTTL = 7 * 24 * time.Hour })
	_ = store.Modify(e.DB, bucketCases, other, func(c *Case, _ bool) error { c.NextCheck = 0; return nil })
	e.watch(context.Background())
	e.Wait()
	if cs := mustCase(t, e, other); cs.State != CaseNoObligation {
		t.Fatalf("expired: %s", cs.State)
	}
}

func ptr[T any](v T) *T { return &v }

func TestRuleOnceAndOnlyTheRealEscrow(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	o.open(t, e, proto.DisputeEvidence{})
	// 100000 − 1000 reserve = 99000; 2% = 1980
	r, err := e.Rule(context.Background(), oid, RuleRequest{User: "49000", Shopper: "48020"})
	if err != nil || r.Split.EscrowFee != "1980" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := e.Rule(context.Background(), oid, RuleRequest{User: "1000", Shopper: "96020"}); !errors.Is(err, ErrAlreadyRuled) {
		t.Fatalf("second ruling: %v", err)
	}

	// the funded output pays another script than the order's P2WSH: no ruling is signed for it
	other := "10000000000000000000000000000000"
	o2 := newOrder(t, e, c, other, strings.Repeat("bb", 32))
	o2.notice(t, e)
	o2.open(t, e, proto.DisputeEvidence{})
	c.set(func(c *chain) { c.txs[o2.txid].Vout[0] = out(t, o2.user.WalletAddress(), 100000) })
	if _, err := e.Rule(context.Background(), other, RuleRequest{User: "49000", Shopper: "48020"}); err == nil || !strings.Contains(err.Error(), "not the escrow") {
		t.Fatalf("rule on a foreign output: %v", err)
	}
}

func TestCountersignedNeedsTheChain(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	o.open(t, e, proto.DisputeEvidence{})
	claim := proto.TxRef{TxID: strings.Repeat("cd", 32)}
	send := func() {
		e.onCountersigned(context.Background(), message(t, o.user.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeDisputeCountersigned, claim))
		e.Wait()
	}
	send() // no ruling yet
	if cs := mustCase(t, e, oid); cs.State != CaseOpen || cs.ClaimedPayout != "" {
		t.Fatalf("%s %s", cs.State, cs.ClaimedPayout)
	}
	if _, err := e.Rule(context.Background(), oid, RuleRequest{User: "49000", Shopper: "48020"}); err != nil {
		t.Fatal(err)
	}
	send()
	if cs := mustCase(t, e, oid); cs.State != CaseRuled || cs.ClaimedPayout != claim.TxID {
		t.Fatalf("a message alone closed the case: %s", cs.State)
	}
	c.set(func(c *chain) { c.outspend[o.txid+":0"] = &btc.Outspend{Spent: true, TxID: claim.TxID} })
	e.watch(context.Background())
	e.Wait()
	if cs := mustCase(t, e, oid); cs.State != CaseClosed || cs.PayoutTx != claim.TxID {
		t.Fatalf("%s %s", cs.State, cs.PayoutTx)
	}
}

func TestDeliveryAddressOnlyFromTheSignedRequest(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	// a key that does not match key_for_escrow_sha256 (e.g. for a ciphertext of the party's choosing)
	k, _ := delivery.NewKey()
	fake, _ := delivery.WrapKey(o.user.NostrSecretHex(), e.Keys.NostrPubHex(), k)
	o.open(t, e, proto.DisputeEvidence{DeliveryKeyForEscrow: fake})
	if cs := mustCase(t, e, oid); cs.DeliveryAddress != nil {
		t.Fatal("decrypted with a key the request does not commit to")
	}
	// the user's signed order.escrow_key among the evidence messages opens the request's ciphertext
	ek := inner(t, o.user, o.shopper.NostrPubHex(), oid, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: o.keyForEscrow})
	e.onEvidence(context.Background(), message(t, o.shopper.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeDisputeEvidence,
		proto.DisputeEvidence{Messages: []*nostr.Event{ek}}))
	if cs := mustCase(t, e, oid); cs.DeliveryAddress == nil || cs.DeliveryAddress.Name != "山田 太郎" {
		t.Fatalf("%+v", cs.DeliveryAddress)
	}
}

func TestAttachments(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	o.open(t, e, proto.DisputeEvidence{})
	shopperSK := o.shopper.NostrSecretHex()
	send := func(a proto.Attachment) {
		e.onAttachment(context.Background(), message(t, shopperSK, e.Keys.NostrPubHex(), oid, proto.TypeAttachment, a))
	}
	data := []byte(strings.Repeat("screenshot ", 3000))
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	half := len(data) / 2

	// malformed: a short hash (this crashed the node), too many chunks, a bad index
	send(proto.Attachment{SHA256: "ab", MIME: "image/png", Index: 0, Total: 1, DataB64: "AA=="})
	send(proto.Attachment{SHA256: sha, MIME: "image/png", Index: 0, Total: maxChunks + 1, DataB64: "AA=="})
	send(proto.Attachment{SHA256: sha, MIME: "image/png", Index: 2, Total: 2, DataB64: "AA=="})
	if at := mustCase(t, e, oid).Attachments; len(at) != 0 {
		t.Fatalf("malformed attachments kept: %+v", at)
	}
	// chunks in any order; the hash is checked when the last one arrives
	send(proto.Attachment{SHA256: sha, MIME: "image/png", Index: 1, Total: 2, DataB64: base64.StdEncoding.EncodeToString(data[half:])})
	send(proto.Attachment{SHA256: sha, MIME: "image/png", Index: 0, Total: 2, DataB64: base64.StdEncoding.EncodeToString(data[:half])})
	cs := mustCase(t, e, oid)
	if at := cs.Attachments[sha]; at == nil || at.DataB64 != base64.StdEncoding.EncodeToString(data) || at.Size != len(data) {
		t.Fatalf("attachment %+v", cs.Attachments)
	}
	// the case document itself stays small
	var raw Case
	_, _ = e.DB.Get(bucketCases, oid, &raw)
	if len(raw.Attachments) != 0 {
		t.Fatal("attachment data stored in the case document")
	}
	// a wrong hash is rejected without crashing
	bad := strings.Repeat("0", 64)
	send(proto.Attachment{SHA256: bad, MIME: "image/png", Index: 0, Total: 1, DataB64: "AA=="})
	if at := mustCase(t, e, oid).Attachments[bad]; at != nil {
		t.Fatalf("attachment with a wrong hash kept: %+v", at)
	}
	// at most maxAttachmentsPerParty items per party
	for i := 0; i < maxAttachmentsPerParty+2; i++ {
		send(proto.Attachment{SHA256: fmt.Sprintf("%064x", i+1), MIME: "image/png", Index: 0, Total: 2, DataB64: "AA=="})
	}
	if n := len(mustCase(t, e, oid).Attachments); n != maxAttachmentsPerParty {
		t.Fatalf("%d attachments kept", n)
	}
	// evidence per party is bounded
	for i := 0; i < maxEvidencePerParty+5; i++ {
		e.onEvidence(context.Background(), message(t, shopperSK, e.Keys.NostrPubHex(), oid, proto.TypeDisputeEvidence, proto.DisputeEvidence{Text: fmt.Sprint(i)}))
	}
	if n := len(mustCase(t, e, oid).Evidence[o.shopper.NostrPubHex()]); n != maxEvidencePerParty {
		t.Fatalf("%d evidence entries", n)
	}
}

// puppetOrder is an agreement for order id between a sock puppet (user-2) and shopper-1, naming the funding
// transaction fundTx of the real order and quoting an upfront fee of fee (review PoC).
func puppetOrder(t *testing.T, e *Engine, o *order, id, fundTx, fee string) (*keys.Set, []*nostr.Event) {
	pu := labKeys(t, "user-2")
	uk, _ := pu.OrderKey(id)
	proof, _ := keys.SignKeyProofBTC(uk, id, pu.NostrPubHex())
	req := proto.OrderRequest{Payment: proto.AssetBTC, Escrow: e.Keys.NostrPubHex(), KeyProof: proof,
		Delivery:      proto.Delivery{Ciphertext: "x", KeyForShopper: "k", KeyForEscrowSHA256: "00"},
		UserBTCPubkey: hex.EncodeToString(uk.PubKey().SerializeCompressed()), UserBTCAddress: pu.WalletAddress()}
	sk, _ := o.shopper.OrderKey(id)
	ek, _ := e.Keys.EscrowOrderKey(id)
	q := proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "1", EscrowUpfrontFee: fee, PayoutFeeReserve: "0",
		Timelock: &proto.Timelock{T1: 1000, T2: 1100}, ShopperBTCPubkey: hex.EncodeToString(sk.PubKey().SerializeCompressed()),
		ShopperBTCAddress: o.shopper.WalletAddress(), EscrowBTCPubkey: hex.EncodeToString(ek.PubKey().SerializeCompressed()),
		EscrowBTCFeeAddress: e.Keys.WalletAddress()}
	esc, _ := contract.BTCEscrow(&req, &q)
	q.EscrowAddress, _ = esc.Address()
	r := inner(t, pu, o.shopper.NostrPubHex(), id, proto.TypeOrderRequest, req)
	qq := inner(t, o.shopper, pu.NostrPubHex(), id, proto.TypeOrderQuote, q)
	a := inner(t, pu, o.shopper.NostrPubHex(), id, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: qq.ID})
	vout := uint32(0)
	f := inner(t, pu, o.shopper.NostrPubHex(), id, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: fundTx, Vout: &vout, FeeTxID: fundTx, Amount: "1"})
	return pu, []*nostr.Event{r, qq, a, f}
}

// Review PoC: a sock puppet of the shopper sends its escrow.notice for the order first. Only the agreement whose
// funding the chain shows paying our key counts; the real user's case opens.
func TestNoticeSquat(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	pu, evs := puppetOrder(t, e, o, oid, strings.Repeat("aa", 32), "1000")
	e.onNotice(context.Background(), message(t, pu.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeEscrowNotice,
		proto.EscrowNotice{Request: evs[0], Quote: evs[1], Accept: evs[2], Funded: evs[3]}))
	o.notice(t, e) // the real user's notice is kept too
	o.open(t, e, proto.DisputeEvidence{Messages: o.events()})
	cs := mustCase(t, e, oid)
	if cs.User != o.user.NostrPubHex() || cs.State != CaseOpen || !cs.Verified || cs.requestID() != o.request.ID {
		t.Fatalf("case user=%s state=%s verified=%v history=%v", cs.User, cs.State, cs.Verified, cs.History)
	}
	if h := strings.Join(cs.History, "\n"); !strings.Contains(h, "conflicting request "+evs[0].ID) {
		t.Fatalf("the puppet's request is not recorded: %s", h)
	}
	// the puppet's dispute changes nothing now
	e.onDisputeOpen(context.Background(), message(t, pu.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeDisputeOpen,
		proto.DisputeOpen{Claim: proto.ClaimOther, Evidence: proto.DisputeEvidence{Messages: evs}}))
	e.Wait()
	if cs := mustCase(t, e, oid); cs.User != o.user.NostrPubHex() || cs.State != CaseOpen {
		t.Fatalf("taken over: %s %s", cs.User, cs.State)
	}
}

// Review PoC: the shopper opens a dispute on a made-up order (quoting no fee) that names the real funding, to
// take the fee of the real order. The made-up order's funding does not verify, so it claims nothing.
func TestFeeSquatZeroFee(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	other := "20000000000000000000000000000000"
	_, evs := puppetOrder(t, e, o, other, strings.Repeat("aa", 32), "0")
	e.onDisputeOpen(context.Background(), message(t, o.shopper.NostrSecretHex(), e.Keys.NostrPubHex(), other, proto.TypeDisputeOpen,
		proto.DisputeOpen{Claim: proto.ClaimOther, Evidence: proto.DisputeEvidence{Messages: evs}}))
	e.Wait()
	if cs := mustCase(t, e, other); cs.State != CaseNoObligation || cs.Verified || cs.FeePaid {
		t.Fatalf("fake case %s verified=%v", cs.State, cs.Verified)
	}
	o.open(t, e, proto.DisputeEvidence{})
	if cs := mustCase(t, e, oid); cs.State != CaseOpen || !cs.FeePaid {
		t.Fatalf("real case %s %v", cs.State, cs.History)
	}
}

// A stranger who opens first leaves an unverified stub; the real order replaces it.
func TestVerifiedAgreementReplacesStub(t *testing.T) {
	e, c := setup(t)
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	pu, evs := puppetOrder(t, e, o, oid, strings.Repeat("aa", 32), "1000")
	e.onDisputeOpen(context.Background(), message(t, pu.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeDisputeOpen,
		proto.DisputeOpen{Claim: proto.ClaimOther, Evidence: proto.DisputeEvidence{Messages: evs}}))
	e.Wait()
	if cs := mustCase(t, e, oid); cs.State != CaseNoObligation || cs.Verified || len(cs.Evidence) != 0 {
		t.Fatalf("stub %+v", cs)
	}
	// evidence for a stub is not stored (it stays in the inbox)
	early := message(t, o.user.NostrSecretHex(), e.Keys.NostrPubHex(), oid, proto.TypeDisputeEvidence, proto.DisputeEvidence{Text: "early"})
	e.onEvidence(context.Background(), early)
	o.open(t, e, proto.DisputeEvidence{Messages: o.events(), Text: "opened"})
	cs := mustCase(t, e, oid)
	if cs.State != CaseOpen || cs.User != o.user.NostrPubHex() || cs.OpenedBy != o.user.NostrPubHex() {
		t.Fatalf("%s %s %v", cs.State, cs.User, cs.History)
	}
	// the opener's evidence and the early evidence were taken from the inbox, once each
	if got := cs.Evidence[o.user.NostrPubHex()]; len(got) != 2 {
		t.Fatalf("evidence %+v", got)
	}
	e.onEvidence(context.Background(), early) // resent: counted once
	if got := mustCase(t, e, oid).Evidence[o.user.NostrPubHex()]; len(got) != 2 {
		t.Fatalf("resent evidence counted again: %d", len(got))
	}
}

func TestNoObligationWithoutOurFee(t *testing.T) {
	for name, fee := range map[string]string{"no fee": "0", "below our minimum": "999"} {
		t.Run(name, func(t *testing.T) {
			e, c := setup(t)
			o := newOrderQ(t, e, c, oid, strings.Repeat("aa", 32), func(q *proto.OrderQuote) { q.EscrowUpfrontFee = fee })
			o.notice(t, e)
			o.open(t, e, proto.DisputeEvidence{})
			if cs := mustCase(t, e, oid); cs.State != CaseNoObligation || !cs.Verified {
				t.Fatalf("%s %v", cs.State, cs.History)
			}
			var owner string
			if ok, _ := e.DB.Get(bucketFeeUses, "btc-fee:"+o.txid, &owner); ok {
				t.Fatalf("fee claimed by %s", owner)
			}
		})
	}
}

func TestRulingIsBroadcastable(t *testing.T) {
	e, c := setup(t)
	// a dust share is refused before anything is recorded
	o := newOrder(t, e, c, oid, strings.Repeat("aa", 32))
	o.notice(t, e)
	o.open(t, e, proto.DisputeEvidence{})
	if _, err := e.Rule(context.Background(), oid, RuleRequest{User: "500", Shopper: "96520"}); err == nil || !strings.Contains(err.Error(), "dust") {
		t.Fatalf("dust share: %v", err)
	}
	if cs := mustCase(t, e, oid); cs.State != CaseOpen || cs.Ruling != nil {
		t.Fatalf("%s", cs.State)
	}
	// a reserve below the minimum relay fee of the payout is refused
	other := "10000000000000000000000000000000"
	o2 := newOrderQ(t, e, c, other, strings.Repeat("bb", 32), func(q *proto.OrderQuote) { q.PayoutFeeReserve = "100" })
	o2.notice(t, e)
	o2.open(t, e, proto.DisputeEvidence{})
	if _, err := e.Rule(context.Background(), other, RuleRequest{User: "49000", Shopper: "48902"}); err == nil || !strings.Contains(err.Error(), "relay fee") {
		t.Fatalf("low reserve: %v", err)
	}
	// a reserve above what users accept is capped (5% of the lock, at least 2000): 30000 → 5000
	third := "30000000000000000000000000000000"
	o3 := newOrderQ(t, e, c, third, strings.Repeat("cc", 32), func(q *proto.OrderQuote) { q.PayoutFeeReserve = "30000" })
	o3.notice(t, e)
	o3.open(t, e, proto.DisputeEvidence{})
	// 100000 − 5000 = 95000; 2% = 1900
	r, err := e.Rule(context.Background(), third, RuleRequest{User: "46550", Shopper: "46550"})
	if err != nil || r.Split.EscrowFee != "1900" {
		t.Fatalf("%+v %v", r, err)
	}
	// both parties got the ruling
	if cs := mustCase(t, e, third); len(cs.RulingSent) != 2 {
		t.Fatalf("ruling sent to %v", cs.RulingSent)
	}
}
