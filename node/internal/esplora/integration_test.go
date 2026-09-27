//go:build integration

package esplora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

func TestAgainstBitcoind(t *testing.T) {
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
	wallet := rpc.Wallet("faucet")

	ix := NewIndex(rpc, keys.BTCParams, testutil.Logger(t))
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(NewServer(ix, testutil.Logger(t)))
	defer hs.Close()
	cl := btc.NewEsplora(hs.URL, nil)
	sync := func() {
		t.Helper()
		if err := ix.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if tip, err := cl.TipHeight(ctx); err != nil || tip < 110 {
		t.Fatalf("tip %d %v", tip, err)
	}

	// a P2WSH of OP_TRUE (spendable without signatures) and a P2WPKH
	witnessScript := []byte{txscript.OP_TRUE}
	h := sha256.Sum256(witnessScript)
	wsh, _ := btcutil.NewAddressWitnessScriptHash(h[:], keys.BTCParams)
	priv, _ := btcec.NewPrivateKey()
	wpkh := keys.P2WPKHAddress(priv.PubKey())

	fundID, err := wallet.SendToAddress(ctx, wsh.EncodeAddress(), faucet.SatsToBTC(100_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.SendToAddress(ctx, wpkh, faucet.SatsToBTC(50_000)); err != nil {
		t.Fatal(err)
	}
	sync()
	utxos, err := cl.AddressUTXOs(ctx, wsh.EncodeAddress())
	if err != nil || len(utxos) != 1 || utxos[0].Status.Confirmed || utxos[0].Value != 100_000 || utxos[0].TxID != fundID {
		t.Fatalf("mempool utxos %+v %v", utxos, err)
	}
	if txs, err := cl.AddressTxs(ctx, wpkh); err != nil || len(txs) != 1 || txs[0].Fee <= 0 {
		t.Fatalf("mempool txs %+v %v", txs, err)
	}

	if _, err := f.Mine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sync()
	utxos, _ = cl.AddressUTXOs(ctx, wsh.EncodeAddress())
	if len(utxos) != 1 || !utxos[0].Status.Confirmed {
		t.Fatalf("confirmed utxos %+v", utxos)
	}
	if n, err := cl.Confirmations(ctx, fundID); err != nil || n != 1 {
		t.Fatalf("confirmations %d %v", n, err)
	}

	// spend the P2WSH to the P2WPKH through POST /tx
	u := utxos[0]
	hash, _ := chainhash.NewHashFromStr(u.TxID)
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(wire.NewTxIn(wire.NewOutPoint(hash, u.Vout), nil, wire.TxWitness{witnessScript}))
	pk, _ := btc.PkScript(wpkh)
	spend.AddTxOut(wire.NewTxOut(99_000, pk))
	spendID, err := cl.Broadcast(ctx, btc.TxHex(spend))
	if err != nil || spendID != spend.TxHash().String() {
		t.Fatalf("broadcast %q %v", spendID, err)
	}
	os, err := cl.Outspend(ctx, u.TxID, u.Vout)
	if err != nil || !os.Spent || os.TxID != spendID {
		t.Fatalf("outspend %+v %v", os, err)
	}
	var info AddressInfo
	getJSON(t, hs.URL+"/address/"+wpkh, &info)
	if info.ChainStats.FundedTxoSum != 50_000 || info.MempoolStats.FundedTxoSum != 99_000 || info.MempoolStats.TxCount != 1 {
		t.Fatalf("stats %+v", info)
	}
	if _, err := cl.Broadcast(ctx, btc.TxHex(spend)+"00"); err == nil {
		t.Fatal("garbage accepted")
	}

	if _, err := f.Mine(ctx, 1); err != nil {
		t.Fatal(err)
	}
	sync()
	st, err := cl.TxStatus(ctx, spendID)
	if err != nil || !st.Confirmed {
		t.Fatalf("spend status %+v %v", st, err)
	}
	tx, err := cl.Tx(ctx, spendID)
	if err != nil || tx.Fee != 1000 || tx.Vin[0].Prevout.ScriptPubKeyType != "v0_p2wsh" || tx.Vin[0].Prevout.ScriptPubKeyAddress != wsh.EncodeAddress() {
		t.Fatalf("spend %+v %v", tx, err)
	}
	if fees, err := cl.FeeEstimates(ctx); err != nil || fees["6"] <= 0 {
		t.Fatalf("fees %v %v", fees, err)
	}

	// faucet API
	fs := httptest.NewServer(f.Handler())
	defer fs.Close()
	body, _ := json.Marshal(map[string]any{"address": wpkh, "sats": 12345})
	res, err := http.Post(fs.URL+"/btc", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || out["txid"] == "" {
		t.Fatalf("faucet /btc %d %v", res.StatusCode, out)
	}
	sync()
	if st, _ := cl.TxStatus(ctx, out["txid"]); st == nil || !st.Confirmed {
		t.Fatal("faucet payment not mined")
	}
	res, err = http.Post(fs.URL+"/btc", "application/json", strings.NewReader(`{"address":"bc1qmainnet","sats":1}`))
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad address: %v %v", res.StatusCode, err)
	}
	var height struct {
		BTC     int64   `json:"btc"`
		EVMTime *uint64 `json:"evm_time"`
	}
	getJSON(t, fs.URL+"/height", &height)
	tip, _ := cl.TipHeight(ctx)
	if height.BTC != tip || height.EVMTime != nil {
		t.Fatalf("height %+v tip %d", height, tip)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
