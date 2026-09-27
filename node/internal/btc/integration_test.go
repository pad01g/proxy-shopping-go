//go:build integration

package btc_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// TestSpendPathsOnBitcoind funds the escrow script and spends it along each of the three paths on a real
// signet bitcoind; the time locked paths are first refused before their height and accepted after it.
func TestSpendPathsOnBitcoind(t *testing.T) {
	url := testutil.StartBitcoind(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rpc, err := bitcoinrpc.New(url)
	if err != nil {
		t.Fatal(err)
	}
	f := faucet.New(faucet.Config{BTC: rpc, Wallet: "faucet", Log: testutil.Logger(t)})
	if err := f.Init(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	wallet := rpc.Wallet("faucet")

	gen := func() *btcec.PrivateKey {
		k, _ := btcec.NewPrivateKey()
		return k
	}
	u, s, e := gen(), gen(), gen()
	height, err := rpc.GetBlockCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	esc := btc.Escrow{User: u.PubKey(), Shopper: s.PubKey(), Escrow: e.PubKey(), T1: uint32(height + 5), T2: uint32(height + 10)}
	addr, err := esc.Address()
	if err != nil {
		t.Fatal(err)
	}

	fund := func() btc.Outpoint {
		t.Helper()
		txid, err := wallet.SendToAddress(ctx, addr, faucet.SatsToBTC(100_000))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Mine(ctx, 1); err != nil {
			t.Fatal(err)
		}
		raw, err := rpc.GetRawTransactionHex(ctx, txid)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := hex.DecodeString(raw)
		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		want, _ := btc.PkScript(addr)
		for i, o := range tx.TxOut {
			if bytes.Equal(o.PkScript, want) {
				return btc.Outpoint{TxID: txid, Vout: uint32(i), Amount: o.Value}
			}
		}
		t.Fatalf("funding %s has no output to %s", txid, addr)
		return btc.Outpoint{}
	}
	payTo := keys.P2WPKHAddress(gen().PubKey())
	build := func(prev btc.Outpoint, path btc.Path, signers ...*btcec.PrivateKey) string {
		t.Helper()
		p, err := esc.NewSpend(prev, []btc.Output{{Address: payTo, Amount: prev.Amount - 1000}}, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range signers {
			if err := btc.Sign(p, k); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := esc.Finalize(p, path)
		if err != nil {
			t.Fatal(err)
		}
		return btc.TxHex(tx)
	}
	send := func(txHex string) error {
		_, err := rpc.SendRawTransaction(ctx, txHex)
		return err
	}

	multi, t1, t2 := fund(), fund(), fund()

	if err := send(build(multi, btc.PathMultisig, u, e)); err != nil {
		t.Fatalf("2-of-3 (user+escrow) rejected: %v", err)
	}

	early1 := build(t1, btc.PathShopperT1, s)
	early2 := build(t2, btc.PathUserT2, u)
	if err := send(early1); err == nil || !strings.Contains(err.Error(), "non-final") {
		t.Fatalf("T1 spend before T1: %v", err)
	}
	mineTo := func(h int64) {
		t.Helper()
		now, _ := rpc.GetBlockCount(ctx)
		if now < h {
			if _, err := f.Mine(ctx, int(h-now)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// the transaction is final in the block after its locktime, so the tip must reach T1
	mineTo(int64(esc.T1))
	if err := send(early1); err != nil {
		t.Fatalf("T1 spend at T1 rejected: %v", err)
	}
	if err := send(early2); err == nil {
		t.Fatal("T2 spend accepted before T2")
	}
	mineTo(int64(esc.T2))
	if err := send(early2); err != nil {
		t.Fatalf("T2 spend at T2 rejected: %v", err)
	}
	if _, err := f.Mine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mempool, _ := rpc.GetRawMempool(ctx)
	if len(mempool) != 0 {
		t.Fatalf("spends not mined: %v", mempool)
	}
}
