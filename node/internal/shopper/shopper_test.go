package shopper

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func testEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	k, err := keys.LoadMnemonicFile(filepath.Join("..", "..", "..", "lab", "keys", "shopper-1.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := messenger.New(messenger.Config{Secret: k.NostrSecretHex(), Pool: nostrnet.NewPool(nil, nil), DB: db})
	st, _ := trust.NewStore(nil)
	coord, op, esc := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	coordPub, _ := nostr.GetPublicKey(coord)
	opPub, _ := nostr.GetPublicKey(op)
	escPub, _ := nostr.GetPublicKey(esc)
	d, _ := trust.NewDelegation(coord, opPub, "ps-lab", 1, false, "")
	l, _ := trust.NewList(op, 1, &trust.List{Network: "ps-lab", Entries: []trust.Entry{{Region: "JP-13", Shopper: k.NostrPubHex(), Escrow: escPub, Shops: []string{"safe-shop.test"}, Payments: []string{"btc-signet"}}}})
	for _, ev := range []*nostr.Event{d, l} {
		if _, err := st.Put(ev); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Shopper{Payments: []string{"btc-signet"}, Currencies: []string{"JPY"}, TrackingPollSeconds: 1}
	e := New(Deps{Keys: k, Messenger: m, Trust: st, Coordinators: []string{coordPub}, Network: "ps-lab", DB: db, Config: cfg,
		BTC: nilChain{}})
	return e, escPub
}

// nilChain satisfies BTCChain; the rejection tests never reach the chain.
type nilChain struct{ BTCChain }

func reason(err error) string {
	var r *rejection
	if errors.As(err, &r) {
		return r.reason
	}
	return "error: " + err.Error()
}

func TestQuoteRejections(t *testing.T) {
	e, escrow := testEngine(t)
	user, _ := keys.FromMnemonic("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	uk, _ := user.OrderKey("000102030405060708090a0b0c0d0e0f")
	base := proto.OrderRequest{
		ShopURL: "https://safe-shop.test/", ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: "A-100", Qty: 1}},
		Payment: proto.AssetBTC, Escrow: escrow, Delivery: proto.Delivery{Ciphertext: "c", KeyForShopper: "k", KeyForEscrow: "k"},
		UserBTCPubkey: hex.EncodeToString(uk.PubKey().SerializeCompressed()), UserBTCAddress: user.WalletAddress(),
	}
	cases := []struct {
		name string
		edit func(r *proto.OrderRequest)
		want string
	}{
		{"payment not offered", func(r *proto.OrderRequest) { r.Payment = proto.AssetUSDC }, proto.RejectPayment},
		{"bad user key", func(r *proto.OrderRequest) { r.UserBTCPubkey = "02" }, proto.RejectInvalid},
		{"no delivery", func(r *proto.OrderRequest) { r.Delivery.KeyForEscrow = "" }, proto.RejectInvalid},
		{"escrow not listed", func(r *proto.OrderRequest) { r.Escrow = user.NostrPubHex() }, proto.RejectTrust},
		{"region not covered", func(r *proto.OrderRequest) { r.ShopRegion = "JP-27" }, proto.RejectRegion},
		{"shop not listed", func(r *proto.OrderRequest) { r.ShopURL = "https://other.test/" }, proto.RejectRegion},
		{"no escrow profile", func(r *proto.OrderRequest) {}, proto.RejectUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			c.edit(&r)
			_, _, _, err := e.buildQuote(context.Background(), &Order{ID: "000102030405060708090a0b0c0d0e0f", Request: r})
			if err == nil || reason(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
		})
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
