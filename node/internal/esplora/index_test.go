package esplora

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

// fakeChain implements Source with synthetic blocks.
type fakeChain struct {
	blocks  []*bitcoinrpc.Block
	mempool map[string]string // txid → hex
	sent    []string
	reject  bool
}

func (f *fakeChain) GetBlockCount(context.Context) (int64, error) {
	return int64(len(f.blocks)) - 1, nil
}
func (f *fakeChain) GetBlockHash(_ context.Context, h int64) (string, error) {
	if h < 0 || h >= int64(len(f.blocks)) {
		return "", &bitcoinrpc.RPCError{Code: -8, Message: "Block height out of range"}
	}
	return f.blocks[h].Hash, nil
}
func (f *fakeChain) GetBlock(_ context.Context, hash string) (*bitcoinrpc.Block, error) {
	for _, b := range f.blocks {
		if b.Hash == hash {
			return b, nil
		}
	}
	return nil, &bitcoinrpc.RPCError{Code: -5, Message: "Block not found"}
}
func (f *fakeChain) GetRawMempool(context.Context) ([]string, error) {
	ids := []string{}
	for id := range f.mempool {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeChain) GetRawTransactionHex(_ context.Context, txid string) (string, error) {
	if h, ok := f.mempool[txid]; ok {
		return h, nil
	}
	return "", &bitcoinrpc.RPCError{Code: -5, Message: "No such mempool or blockchain transaction"}
}
func (f *fakeChain) SendRawTransaction(_ context.Context, h string) (string, error) {
	if f.reject {
		return "", &bitcoinrpc.RPCError{Code: -26, Message: "non-mandatory-script-verify-flag"}
	}
	tx, err := decodeTx(h)
	if err != nil {
		return "", err
	}
	f.mempool[tx.TxHash().String()] = h
	f.sent = append(f.sent, h)
	return tx.TxHash().String(), nil
}
func (f *fakeChain) EstimateSmartFee(context.Context, int) (*bitcoinrpc.SmartFee, error) {
	return &bitcoinrpc.SmartFee{Errors: []string{"Insufficient data or no feerate found"}}, nil
}

func txHex(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes())
}

func (f *fakeChain) mine(tag byte, txs ...*wire.MsgTx) {
	h := int64(len(f.blocks))
	var prev string
	if h > 0 {
		prev = f.blocks[h-1].Hash
	}
	hash := chainhash.DoubleHashH([]byte(fmt.Sprintf("%s/%d/%d", prev, h, tag)))
	b := &bitcoinrpc.Block{BlockHeader: bitcoinrpc.BlockHeader{Hash: hash.String(), Height: h, Time: 1700000000 + h, PreviousBlockHash: prev}}
	for _, tx := range txs {
		b.Tx = append(b.Tx, bitcoinrpc.BlockTx{TxID: tx.TxHash().String(), Hex: txHex(tx)})
		delete(f.mempool, tx.TxHash().String())
	}
	f.blocks = append(f.blocks, b)
}

func coinbase(height int64, value int64, script []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex}, SignatureScript: []byte{byte(height), 0x51}, Sequence: wire.MaxTxInSequenceNum})
	tx.AddTxOut(wire.NewTxOut(value, script))
	return tx
}

func spendTx(from *wire.MsgTx, vout uint32, outs ...*wire.TxOut) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	in := wire.NewTxIn(wire.NewOutPoint(ptr(from.TxHash()), vout), nil, [][]byte{{1, 2}, {3}})
	tx.AddTxIn(in)
	for _, o := range outs {
		tx.AddTxOut(o)
	}
	return tx
}

func ptr(h chainhash.Hash) *chainhash.Hash { return &h }

type fixture struct {
	chain                      *fakeChain
	p2wpkh, p2wsh, p2tr, p2pkh btcutil.Address
	p2sh                       btcutil.Address
	cb, fund                   *wire.MsgTx
	wpkhScript, wshScript, tr  []byte
	pkhScript, shScript, opRet []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	p := keys.BTCParams
	f := &fixture{chain: &fakeChain{mempool: map[string]string{}}}
	var err error
	must := func(a btcutil.Address, e error) btcutil.Address {
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	f.p2wpkh = must(btcutil.NewAddressWitnessPubKeyHash(bytes.Repeat([]byte{1}, 20), p))
	f.p2wsh = must(btcutil.NewAddressWitnessScriptHash(bytes.Repeat([]byte{2}, 32), p))
	f.p2tr = must(btcutil.NewAddressTaproot(bytes.Repeat([]byte{3}, 32), p))
	f.p2pkh = must(btcutil.NewAddressPubKeyHash(bytes.Repeat([]byte{4}, 20), p))
	f.p2sh = must(btcutil.NewAddressScriptHashFromHash(bytes.Repeat([]byte{5}, 20), p))
	script := func(a btcutil.Address) []byte {
		s, e := txscript.PayToAddrScript(a)
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	f.wpkhScript, f.wshScript, f.tr = script(f.p2wpkh), script(f.p2wsh), script(f.p2tr)
	f.pkhScript, f.shScript = script(f.p2pkh), script(f.p2sh)
	f.opRet, err = txscript.NullDataScript([]byte("ps"))
	if err != nil {
		t.Fatal(err)
	}

	f.chain.mine(0, coinbase(0, 50_0000_0000, f.pkhScript))
	f.cb = coinbase(1, 50_0000_0000, f.wpkhScript)
	f.chain.mine(0, f.cb)
	// block 2: fund the P2WSH (and a P2SH), change back to the P2WPKH
	f.fund = spendTx(f.cb, 0, wire.NewTxOut(100_000, f.wshScript), wire.NewTxOut(49_0000_0000, f.wpkhScript), wire.NewTxOut(5000, f.shScript), wire.NewTxOut(0, f.opRet))
	f.chain.mine(0, coinbase(2, 50_0000_0000, f.pkhScript), f.fund)
	return f
}

func get(t *testing.T, h http.Handler, path string, v any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if v != nil && rec.Code == http.StatusOK {
		if s, ok := v.(*string); ok {
			*s = rec.Body.String()
		} else if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("%s: %v: %s", path, err, rec.Body)
		}
	}
	return rec.Code
}

func TestIndexAndAPI(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ix := NewIndex(f.chain, keys.BTCParams, nil)
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ix, nil)

	var tip string
	if get(t, srv, "/blocks/tip/height", &tip); tip != "2" {
		t.Fatalf("tip %q", tip)
	}

	var tx Tx
	get(t, srv, "/tx/"+f.fund.TxHash().String(), &tx)
	if tx.Fee != 50_0000_0000-49_0000_0000-100_000-5000 || !tx.Status.Confirmed || tx.Status.BlockHeight != 2 {
		t.Fatalf("tx %+v", tx)
	}
	types := []string{"v0_p2wsh", "v0_p2wpkh", "p2sh", "op_return"}
	for i, o := range tx.Vout {
		if o.ScriptPubKeyType != types[i] {
			t.Errorf("vout %d type %s", i, o.ScriptPubKeyType)
		}
	}
	if tx.Vout[0].ScriptPubKeyAddress != f.p2wsh.EncodeAddress() || tx.Vout[2].ScriptPubKeyAddress != f.p2sh.EncodeAddress() || tx.Vout[3].ScriptPubKeyAddress != "" {
		t.Fatalf("addresses %+v", tx.Vout)
	}
	if tx.Vin[0].Prevout == nil || tx.Vin[0].Prevout.ScriptPubKeyAddress != f.p2wpkh.EncodeAddress() || len(tx.Vin[0].Witness) != 2 {
		t.Fatalf("vin %+v", tx.Vin[0])
	}
	var cbtx Tx
	get(t, srv, "/tx/"+f.cb.TxHash().String(), &cbtx)
	if !cbtx.Vin[0].IsCoinbase || cbtx.Fee != 0 || cbtx.Vin[0].Prevout != nil {
		t.Fatalf("coinbase %+v", cbtx.Vin[0])
	}

	// mempool spend of the P2WSH output to a P2TR
	spend := spendTx(f.fund, 0, wire.NewTxOut(99_000, f.tr))
	f.chain.mempool[spend.TxHash().String()] = txHex(spend)
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var utxo []UTXO
	get(t, srv, "/address/"+f.p2wsh.EncodeAddress()+"/utxo", &utxo)
	if len(utxo) != 0 {
		t.Fatalf("output spent in the mempool is listed: %+v", utxo)
	}
	get(t, srv, "/address/"+f.p2tr.EncodeAddress()+"/utxo", &utxo)
	if len(utxo) != 1 || utxo[0].Status.Confirmed || utxo[0].Value != 99_000 {
		t.Fatalf("mempool utxo %+v", utxo)
	}
	var os Outspend
	get(t, srv, fmt.Sprintf("/tx/%s/outspend/0", f.fund.TxHash()), &os)
	if !os.Spent || os.TxID != spend.TxHash().String() || os.Status == nil || os.Status.Confirmed {
		t.Fatalf("outspend %+v", os)
	}
	var info AddressInfo
	get(t, srv, "/address/"+f.p2wsh.EncodeAddress(), &info)
	if info.ChainStats.FundedTxoSum != 100_000 || info.ChainStats.TxCount != 1 || info.MempoolStats.SpentTxoSum != 100_000 || info.MempoolStats.TxCount != 1 {
		t.Fatalf("stats %+v", info)
	}
	var txs []Tx
	get(t, srv, "/address/"+f.p2wsh.EncodeAddress()+"/txs", &txs)
	if len(txs) != 2 || txs[0].TxID != spend.TxHash().String() || txs[0].Vin[0].InnerWitnessScriptAsm == "" {
		t.Fatalf("txs %+v", txs)
	}

	// mined
	f.chain.mine(0, coinbase(3, 50_0000_0000, f.pkhScript), spend)
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	get(t, srv, "/address/"+f.p2tr.EncodeAddress()+"/utxo", &utxo)
	if len(utxo) != 1 || !utxo[0].Status.Confirmed || utxo[0].Status.BlockHeight != 3 {
		t.Fatalf("confirmed utxo %+v", utxo)
	}
	var outs []Outspend
	get(t, srv, fmt.Sprintf("/tx/%s/outspends", f.fund.TxHash()), &outs)
	if len(outs) != 4 || !outs[0].Spent || !outs[0].Status.Confirmed || outs[1].Spent {
		t.Fatalf("outspends %+v", outs)
	}

	// reorg: block 3 is replaced by one without the spend, which returns to the mempool
	f.chain.blocks = f.chain.blocks[:3]
	f.chain.mine(9, coinbase(3, 50_0000_0000, f.pkhScript))
	f.chain.mine(9, coinbase(4, 50_0000_0000, f.pkhScript))
	f.chain.mempool[spend.TxHash().String()] = txHex(spend)
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var st Status
	get(t, srv, "/tx/"+spend.TxHash().String()+"/status", &st)
	if st.Confirmed {
		t.Fatalf("reorged tx still confirmed: %+v", st)
	}
	var hash string
	get(t, srv, "/block-height/3", &hash)
	if hash != f.chain.blocks[3].Hash {
		t.Fatal("block 3 not replaced")
	}
	get(t, srv, "/address/"+f.p2tr.EncodeAddress(), &info)
	if info.ChainStats.TxCount != 0 || info.MempoolStats.FundedTxoCount != 1 {
		t.Fatalf("stats after reorg %+v", info)
	}

	if code := get(t, srv, "/tx/"+strings.Repeat("00", 32), nil); code != http.StatusNotFound {
		t.Fatalf("unknown tx: %d", code)
	}
	if code := get(t, srv, "/address/bc1qnotsignet/utxo", nil); code != http.StatusBadRequest {
		t.Fatalf("bad address: %d", code)
	}
	var fees map[string]float64
	get(t, srv, "/fee-estimates", &fees)
	if fees["1"] != 1 || fees["144"] != 1 || len(fees) != len(feeTargets) {
		t.Fatalf("fees %v", fees)
	}
}

func TestBroadcastAndCORS(t *testing.T) {
	f := newFixture(t)
	ix := NewIndex(f.chain, keys.BTCParams, nil)
	if err := ix.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ix, nil)
	spend := spendTx(f.fund, 1, wire.NewTxOut(1000, f.pkhScript))

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tx", strings.NewReader(body)))
		return rec
	}
	rec := post(txHex(spend) + "\n")
	if rec.Code != http.StatusOK || rec.Body.String() != spend.TxHash().String() || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("broadcast %d %s", rec.Code, rec.Body)
	}
	// visible immediately
	var utxo []UTXO
	get(t, srv, "/address/"+f.p2pkh.EncodeAddress()+"/utxo", &utxo)
	found := false
	for _, u := range utxo {
		found = found || u.TxID == spend.TxHash().String()
	}
	if !found {
		t.Fatal("broadcast tx not indexed")
	}
	f.chain.reject = true
	if rec := post(txHex(spend)); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "-26") {
		t.Fatalf("rejection %d %s", rec.Code, rec.Body)
	}
	if rec := post("zz"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad hex %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/tx", nil))
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight %d", rec.Code)
	}
}

func TestBlockMustFollowTip(t *testing.T) {
	ix := NewIndex(&fakeChain{}, keys.BTCParams, nil)
	err := ix.addBlock(&bitcoinrpc.Block{BlockHeader: bitcoinrpc.BlockHeader{Height: 5}})
	if err == nil {
		t.Fatal("gap accepted")
	}
	var re *bitcoinrpc.RPCError
	if errors.As(err, &re) {
		t.Fatal("unexpected rpc error")
	}
}
