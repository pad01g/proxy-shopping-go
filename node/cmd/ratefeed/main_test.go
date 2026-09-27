package main

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
	"github.com/pad01g/proxy-shopping-go/node/internal/fx"
)

func TestScale8(t *testing.T) {
	for in, want := range map[string]int64{
		"100000": 100000_00000000, "1/150": 666667, "1": 100000000, "0.000000004": 0, "0.000000005": 1,
	} {
		r, _ := new(big.Rat).SetString(in)
		if got := Scale8(r); got.Int64() != want {
			t.Errorf("Scale8(%s) = %s, want %d", in, got, want)
		}
	}
}

func TestFeederWritesOnlyChanges(t *testing.T) {
	rates := map[string]string{"BTC/USD": "100000", "USD/JPY": "150", "USDC/USD": "1"}
	svc := fx.New([]fx.Provider{fx.Static(rates)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var writes []string
	f := &feeder{
		rates: svc,
		feeds: map[string]common.Address{"BTC/USD": common.HexToAddress("0x1"), "JPY/USD": common.HexToAddress("0x2"), "USDC/USD": common.HexToAddress("0x3")},
		last:  map[string]*big.Int{},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		write: func(_ context.Context, feed common.Address, answer *big.Int) error {
			writes = append(writes, feed.Hex()[40:]+":"+answer.String())
			return nil
		},
	}
	ctx := context.Background()
	f.update(ctx)
	if len(writes) != 3 || writes[1] != "02:666667" {
		t.Fatalf("first run wrote %v", writes)
	}
	f.update(ctx)
	if len(writes) != 3 {
		t.Fatalf("unchanged values written again: %v", writes)
	}
	// a new rate is written; the others stay
	f.rates = fx.New([]fx.Provider{fx.Static(map[string]string{"BTC/USD": "90000", "USD/JPY": "150", "USDC/USD": "1"})}, nil)
	f.update(ctx)
	if len(writes) != 4 || writes[3] != "01:9000000000000" {
		t.Fatalf("after change %v", writes)
	}
}

// The feeder must not share labfaucet's (and the deployer's) anvil account: two senders of one account race
// for the same nonces.
func TestDefaultKeyIsNotTheFaucets(t *testing.T) {
	addr := func(hexKey string) common.Address {
		k, err := loadKey(hexKey)
		if err != nil {
			t.Fatal(err)
		}
		return crypto.PubkeyToAddress(*k.PubKey().ToECDSA())
	}
	if got := addr(""); got != common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8") {
		t.Fatalf("default sender %s, want anvil account 1", got.Hex())
	}
	if addr("") == addr(faucet.AnvilKey0) {
		t.Fatal("ratefeed sends from the faucet's account")
	}
	if _, err := loadKey("0x1234"); err == nil {
		t.Fatal("short key accepted")
	}
}
