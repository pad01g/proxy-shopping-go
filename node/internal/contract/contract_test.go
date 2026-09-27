package contract

import (
	"encoding/hex"
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

func TestFromEvents(t *testing.T) {
	user, shopper, mallory := person(), person(), person()
	req := msg(t, user, shopper, proto.TypeOrderRequest, proto.OrderRequest{Payment: proto.AssetBTC})
	quote := msg(t, shopper, user, proto.TypeOrderQuote, proto.OrderQuote{Accept: true, LockAmount: "10"})
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
