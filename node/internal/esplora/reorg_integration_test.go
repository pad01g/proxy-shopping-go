//go:build integration

package esplora

import (
	"context"
	"crypto/sha256"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// TestReorgAgainstBitcoind orphans a block with invalidateblock and mines a competing chain: the index must
// follow the node's chain block by block and report the payment as unconfirmed, then confirmed in the new block.
func TestReorgAgainstBitcoind(t *testing.T) {
	url := testutil.StartBitcoind(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rpc, err := bitcoinrpc.New(url)
	if err != nil {
		t.Fatal(err)
	}
	f := faucet.New(faucet.Config{BTC: rpc, Wallet: "faucet", Log: testutil.Logger(t)})
	if err := f.Init(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(rpc, keys.BTCParams, testutil.Logger(t))
	hs := httptest.NewServer(NewServer(ix, testutil.Logger(t)))
	defer hs.Close()
	cl := btc.NewEsplora(hs.URL, nil)
	sync := func() {
		t.Helper()
		if err := ix.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sameChain := func() {
		t.Helper()
		count, err := rpc.GetBlockCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if h, _ := ix.Tip(); h != count {
			t.Fatalf("indexed tip %d, node %d", h, count)
		}
		for h := count; h > count-5 && h >= 0; h-- {
			want, _ := rpc.GetBlockHash(ctx, h)
			if got, _ := ix.BlockHash(h); got != want {
				t.Fatalf("block %d: indexed %s, node %s", h, got, want)
			}
		}
	}

	script := []byte{txscript.OP_TRUE}
	h := sha256.Sum256(script)
	wsh, _ := btcutil.NewAddressWitnessScriptHash(h[:], keys.BTCParams)
	txid, err := rpc.Wallet("faucet").SendToAddress(ctx, wsh.EncodeAddress(), faucet.SatsToBTC(100_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Mine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sync()
	st, err := cl.TxStatus(ctx, txid)
	if err != nil || !st.Confirmed {
		t.Fatalf("status %+v %v", st, err)
	}
	orphaned, orphanHeight := st.BlockHash, st.BlockHeight

	// the block goes away: its payment returns to the mempool
	if err := rpc.Call(ctx, "invalidateblock", []any{orphaned}, nil); err != nil {
		t.Fatal(err)
	}
	sync()
	sameChain()
	if st, err := cl.TxStatus(ctx, txid); err != nil || st.Confirmed {
		t.Fatalf("payment of the invalidated block: %+v %v", st, err)
	}
	if utxos, _ := cl.AddressUTXOs(ctx, wsh.EncodeAddress()); len(utxos) != 1 || utxos[0].Status.Confirmed {
		t.Fatalf("utxos after invalidateblock %+v", utxos)
	}

	// a competing, longer chain; the node mines the payment again, in another block. Mined within the same
	// second it would be the invalidated block again (same transactions, time and coinbase), which is refused.
	time.Sleep(1100 * time.Millisecond)
	if _, err := f.Mine(ctx, 2); err != nil {
		t.Fatal(err)
	}
	sync()
	sameChain()
	st, err = cl.TxStatus(ctx, txid)
	if err != nil || !st.Confirmed || st.BlockHash == orphaned || st.BlockHeight != orphanHeight {
		t.Fatalf("payment in the new chain: %+v %v (orphaned %s at %d)", st, err, orphaned, orphanHeight)
	}
	if n, err := cl.Confirmations(ctx, txid); err != nil || n != 2 {
		t.Fatalf("confirmations %d %v", n, err)
	}
}
