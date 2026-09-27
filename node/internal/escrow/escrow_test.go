package escrow

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	k, err := keys.LoadMnemonicFile(filepath.Join("..", "..", "..", "lab", "keys", "escrow-1.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := messenger.New(messenger.Config{Secret: k.NostrSecretHex(), Pool: nostrnet.NewPool(nil, nil), DB: db})
	return New(Deps{Keys: k, Messenger: m, DB: db, Config: &config.Escrow{
		UpfrontFee: config.UpfrontFee{BPS: 50, MinSats: "1000", MinUSDC: "0.50"}, DisputeFeeBPS: 200}, Name: "escrow-1"})
}

func TestSplit(t *testing.T) {
	e := testEngine(t)
	fee, err := e.split(big.NewInt(89600), big.NewInt(40000), big.NewInt(47808))
	if err != nil || fee.Int64() != 1792 {
		t.Fatalf("fee %v %v", fee, err)
	}
	if _, err := e.split(big.NewInt(89600), big.NewInt(40000), big.NewInt(49600)); err == nil {
		t.Fatal("split ignoring the dispute fee accepted")
	}
}

func TestProfileAndNoObligation(t *testing.T) {
	e := testEngine(t)
	p, err := e.Profile(nil)
	if err != nil || p.BTCXpub == "" || p.BTCFeeAddress != e.Keys.WalletAddress() || p.DisputeFeeBPS != 200 || p.UpfrontFee.MinSats != "1000" {
		t.Fatalf("profile %+v %v", p, err)
	}
	_ = e.DB.Put(bucketCases, "000102030405060708090a0b0c0d0e0f", &Case{OrderID: "000102030405060708090a0b0c0d0e0f", State: CaseNoObligation})
	_, err = e.Rule(context.Background(), "000102030405060708090a0b0c0d0e0f", RuleRequest{User: "1", Shopper: "1"})
	if !errors.Is(err, ErrNoObligation) {
		t.Fatalf("rule on an unpaid case: %v", err)
	}
	if _, err := e.Rule(context.Background(), "ffffffffffffffffffffffffffffffff", RuleRequest{}); err == nil {
		t.Fatal("rule without a case")
	}
}
