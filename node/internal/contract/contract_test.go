package contract

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

const oid = "000102030405060708090a0b0c0d0e0f"

type who struct{ sk, pk string }

func person() who {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	return who{sk, pk}
}

func msg(t *testing.T, from who, to who, typ string, body any) *nostr.Event {
	t.Helper()
	ev, err := giftwrap.NewInner(from.sk, to.pk, oid, typ, body, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// btcRequest is a BTC request of who with a key proof of the chain key k.
func btcRequest(t *testing.T, k *btcec.PrivateKey, who who) proto.OrderRequest {
	t.Helper()
	proof, err := keys.SignKeyProofBTC(k, oid, who.pk)
	if err != nil {
		t.Fatal(err)
	}
	return proto.OrderRequest{Payment: proto.AssetBTC, UserBTCPubkey: hex.EncodeToString(k.PubKey().SerializeCompressed()), KeyProof: proof}
}

func TestFromEvents(t *testing.T) {
	user, shopper, mallory := person(), person(), person()
	uk, _ := btcec.NewPrivateKey()
	req := msg(t, user, shopper, proto.TypeOrderRequest, btcRequest(t, uk, user))
	quote := msg(t, shopper, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "10"})
	accept := msg(t, user, shopper, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: quote.ID})
	vout := uint32(0)
	funded := msg(t, user, shopper, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: "aa", Vout: &vout})

	o, err := FromEvents([]*nostr.Event{funded, accept, quote, req})
	if err != nil {
		t.Fatal(err)
	}
	if o.User != user.pk || o.Shopper != shopper.pk || o.Funded == nil || *o.Funded.Vout != 0 || len(o.Events()) != 4 {
		t.Fatalf("%+v", o)
	}

	forged := msg(t, mallory, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true})
	if _, err := FromEvents([]*nostr.Event{req, forged}); err == nil {
		t.Fatal("quote by a stranger accepted")
	}
	otherAccept := msg(t, user, shopper, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: forged.ID})
	if _, err := FromEvents([]*nostr.Event{req, quote, otherAccept}); err == nil {
		t.Fatal("accept of another quote accepted")
	}
	strangerFunded := msg(t, mallory, shopper, proto.TypeOrderFunded, proto.OrderFunded{})
	if _, err := FromEvents([]*nostr.Event{req, quote, strangerFunded}); err == nil {
		t.Fatal("funded by a stranger accepted")
	}
	tampered := *quote
	tampered.Content = `{"accept":true,"lock_amount":"1"}`
	if _, err := FromEvents([]*nostr.Event{req, &tampered}); err == nil {
		t.Fatal("tampered quote accepted")
	}
	if _, err := FromEvents([]*nostr.Event{req}); err == nil {
		t.Fatal("request alone accepted")
	}

	// mallory copies the user's chain key into her own request: she cannot prove she holds it (§4.4.1)
	copied := btcRequest(t, uk, user)
	stolen := msg(t, mallory, shopper, proto.TypeOrderRequest, copied)
	mq := msg(t, shopper, mallory, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC})
	if _, err := FromEvents([]*nostr.Event{stolen, mq}); err == nil || !strings.Contains(err.Error(), "key_proof") {
		t.Fatalf("request with a copied chain key: %v", err)
	}
	noProof := copied
	noProof.KeyProof = ""
	if _, err := FromEvents([]*nostr.Event{msg(t, user, shopper, proto.TypeOrderRequest, noProof), quote}); err == nil {
		t.Fatal("request without key_proof accepted")
	}
	// the quote must be for the requested payment
	usdcQuote := msg(t, shopper, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetUSDC})
	if _, err := FromEvents([]*nostr.Event{req, usdcQuote}); err == nil {
		t.Fatal("quote for another payment accepted")
	}
}

func TestSelect(t *testing.T) {
	user, shopper, mallory := person(), person(), person()
	uk, _ := btcec.NewPrivateKey()
	req := msg(t, user, shopper, proto.TypeOrderRequest, btcRequest(t, uk, user))
	quote := msg(t, shopper, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "10"})
	accept := msg(t, user, shopper, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: quote.ID})
	vout := uint32(0)
	funded := msg(t, user, shopper, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: "aa", Vout: &vout})
	notice := []*nostr.Event{req, quote, accept, funded}

	// mallory's own request and quote, put into the evidence of a dispute
	mk, _ := btcec.NewPrivateKey()
	mreq := msg(t, mallory, shopper, proto.TypeOrderRequest, btcRequest(t, mk, mallory))
	mquote := msg(t, shopper, mallory, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "1"})

	// the notice wins; the differing evidence is reported
	evs, ignored, err := Select(oid, notice, []*nostr.Event{mreq, mquote, req})
	if err != nil || len(ignored) != 2 {
		t.Fatalf("%v %v", ignored, err)
	}
	o, err := FromEvents(evs)
	if err != nil || o.User != user.pk || o.Quote.LockAmount != "10" {
		t.Fatalf("%+v %v", o, err)
	}
	// without a notice, two different requests for one order are refused
	if _, _, err := Select(oid, nil, []*nostr.Event{req, quote, mreq, mquote}); err == nil {
		t.Fatal("conflicting requests accepted")
	}
	// without a notice, the quote named by the accept is used, and the latest funding
	later := *funded
	later2 := msg(t, user, shopper, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: "bb", Vout: &vout})
	later2.CreatedAt = later.CreatedAt + 10
	_ = later2.Sign(user.sk)
	other := msg(t, shopper, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, Asset: proto.AssetBTC, LockAmount: "99"})
	evs, _, err = Select(oid, nil, []*nostr.Event{other, req, quote, accept, funded, later2})
	if err != nil {
		t.Fatal(err)
	}
	o, err = FromEvents(evs)
	if err != nil || o.Quote.LockAmount != "10" || o.Funded.TxID != "bb" {
		t.Fatalf("%+v %v", o, err)
	}
}

// chain is a BTCChain stand-in.
type chain struct {
	tx   *btc.Tx
	err  error
	conf int64
}

func (c *chain) Tx(context.Context, string) (*btc.Tx, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.tx == nil {
		return nil, &btc.HTTPError{Status: 404}
	}
	return c.tx, nil
}

func (c *chain) Confirmations(context.Context, string) (int64, error) { return c.conf, nil }

func TestVerifyBTCFundingErrors(t *testing.T) {
	gen := func() *btcec.PrivateKey { k, _ := btcec.NewPrivateKey(); return k }
	esc := btc.Escrow{User: gen().PubKey(), Shopper: gen().PubKey(), Escrow: gen().PubKey(), T1: 200, T2: 300}
	addr, _ := esc.Address()
	feeAddr := keys.P2WPKHAddress(gen().PubKey())
	q := &proto.OrderQuote{LockAmount: "1000", EscrowUpfrontFee: "100", EscrowAddress: addr, EscrowBTCFeeAddress: feeAddr}
	txid := strings.Repeat("ab", 32)
	vout := uint32(0)
	f := &proto.OrderFunded{Asset: proto.AssetBTC, TxID: txid, Vout: &vout, FeeTxID: txid}
	out := func(a string, v int64) btc.TxOut {
		pk, _ := btc.PkScript(a)
		return btc.TxOut{ScriptPubKey: hex.EncodeToString(pk), ScriptPubKeyAddress: a, Value: v}
	}
	good := &btc.Tx{TxID: txid, Vout: []btc.TxOut{out(addr, 1000), out(feeAddr, 100)}, Status: btc.TxStatus{Confirmed: true, BlockTime: 1790000000}}
	ctx := context.Background()

	fnd, err := VerifyBTCFunding(ctx, &chain{tx: good, conf: 1}, esc, q, f, 1)
	if err != nil || fnd.Outpoint.Amount != 1000 || fnd.ConfirmedAt != 1790000000 || len(fnd.Uses) != 2 {
		t.Fatalf("%+v %v", fnd, err)
	}
	// transient: not indexed yet, not confirmed, Esplora failing
	for name, c := range map[string]*chain{
		"404": {}, "unconfirmed": {tx: good, conf: 0}, "5xx": {err: &btc.HTTPError{Status: 502}}, "network": {err: errors.New("dial tcp: refused")},
	} {
		if _, err := VerifyBTCFunding(ctx, c, esc, q, f, 1); err == nil || IsDefinite(err) {
			t.Errorf("%s: %v must be retried", name, err)
		}
	}
	// definite: another script (the address field alone is not trusted), too little, fee missing or elsewhere
	spoofed := *good
	spoofed.Vout = []btc.TxOut{{ScriptPubKey: "0014" + strings.Repeat("00", 20), ScriptPubKeyAddress: addr, Value: 1000}, out(feeAddr, 100)}
	short := *good
	short.Vout = []btc.TxOut{out(addr, 999), out(feeAddr, 100)}
	noFee := *good
	noFee.Vout = []btc.TxOut{out(addr, 1000)}
	for name, c := range map[string]*chain{"script": {tx: &spoofed, conf: 1}, "amount": {tx: &short, conf: 1}, "fee": {tx: &noFee, conf: 1}} {
		if _, err := VerifyBTCFunding(ctx, c, esc, q, f, 1); !IsDefinite(err) {
			t.Errorf("%s: %v must be definite", name, err)
		}
	}
	elsewhere := *f
	elsewhere.FeeTxID = strings.Repeat("cd", 32)
	if _, err := VerifyBTCFunding(ctx, &chain{tx: good, conf: 1}, esc, q, &elsewhere, 1); !IsDefinite(err) {
		t.Errorf("fee in another tx: %v", err)
	}
	if _, err := VerifyBTCFee(ctx, &chain{tx: good}, q, &elsewhere); !IsDefinite(err) {
		t.Errorf("escrow: fee in another tx: %v", err)
	}
	if use, err := VerifyBTCFee(ctx, &chain{tx: good}, q, f); err != nil || use != "btc-fee:"+txid {
		t.Errorf("escrow fee check: %s %v", use, err)
	}
}

func TestCheckBTCPayout(t *testing.T) {
	gen := func() *btcec.PrivateKey { k, _ := btcec.NewPrivateKey(); return k }
	u, s, e := gen(), gen(), gen()
	esc := btc.Escrow{User: u.PubKey(), Shopper: s.PubKey(), Escrow: e.PubKey(), T1: 200, T2: 300}
	prev := btc.Outpoint{TxID: strings.Repeat("cd", 32), Vout: 0, Amount: 90600}
	shopperAddr, userAddr := keys.P2WPKHAddress(s.PubKey()), keys.P2WPKHAddress(u.PubKey())
	build := func(outs []btc.Output, signer *btcec.PrivateKey) string {
		p, err := esc.NewSpend(prev, outs, btc.PathMultisig)
		if err != nil {
			t.Fatal(err)
		}
		if signer != nil {
			_ = btc.Sign(p, signer)
		}
		b, _ := btc.EncodePSBT(p)
		return b
	}
	check := func(b64 string, want map[string]int64, extra map[string]int64, signedBy *btcec.PublicKey) error {
		p, err := btc.DecodePSBT(b64)
		if err != nil {
			t.Fatal(err)
		}
		return CheckBTCPayout(p, esc, prev, want, extra, 1000, signedBy)
	}
	good := build([]btc.Output{{Address: shopperAddr, Amount: 89600}}, u)
	if err := check(good, map[string]int64{shopperAddr: 89600}, nil, u.PubKey()); err != nil {
		t.Fatal(err)
	}
	if err := check(good, map[string]int64{shopperAddr: 89600}, nil, e.PubKey()); err == nil {
		t.Fatal("missing escrow signature not noticed")
	}
	short := build([]btc.Output{{Address: shopperAddr, Amount: 80000}}, u)
	if err := check(short, map[string]int64{shopperAddr: 89600}, nil, u.PubKey()); err == nil {
		t.Fatal("short payout accepted")
	}
	skim := build([]btc.Output{{Address: shopperAddr, Amount: 89600}, {Address: userAddr, Amount: 500}}, u)
	if err := check(skim, map[string]int64{shopperAddr: 89600}, nil, u.PubKey()); err == nil {
		t.Fatal("unexpected output accepted")
	}
	if err := check(skim, map[string]int64{shopperAddr: 89600}, map[string]int64{userAddr: 500}, u.PubKey()); err != nil {
		t.Fatalf("allowed extra output refused: %v", err)
	}
	fat := build([]btc.Output{{Address: shopperAddr, Amount: 60000}}, u)
	if err := check(fat, map[string]int64{shopperAddr: 60000}, nil, u.PubKey()); err == nil {
		t.Fatal("fee above the reserve accepted")
	}

	req := &proto.OrderRequest{UserBTCPubkey: hex.EncodeToString(u.PubKey().SerializeCompressed())}
	q := &proto.OrderQuote{ShopperBTCPubkey: hex.EncodeToString(s.PubKey().SerializeCompressed()),
		EscrowBTCPubkey: hex.EncodeToString(e.PubKey().SerializeCompressed()), Timelock: &proto.Timelock{T1: 200, T2: 300}}
	rebuilt, err := BTCEscrow(req, q)
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := rebuilt.Address()
	a2, _ := esc.Address()
	if a1 != a2 {
		t.Fatal("BTCEscrow rebuilt another script")
	}
	q.Timelock.T2 = 100
	if _, err := BTCEscrow(req, q); err == nil {
		t.Fatal("t2 < t1 accepted")
	}
}
