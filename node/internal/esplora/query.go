package esplora

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Status is the Esplora confirmation status.
type Status struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight int64  `json:"block_height,omitempty"`
	BlockHash   string `json:"block_hash,omitempty"`
	BlockTime   int64  `json:"block_time,omitempty"`
}

// Vout is an output in Esplora format.
type Vout struct {
	ScriptPubKey        string `json:"scriptpubkey"`
	ScriptPubKeyAsm     string `json:"scriptpubkey_asm"`
	ScriptPubKeyType    string `json:"scriptpubkey_type"`
	ScriptPubKeyAddress string `json:"scriptpubkey_address,omitempty"`
	Value               int64  `json:"value"`
}

// Vin is an input in Esplora format.
type Vin struct {
	TxID                  string   `json:"txid"`
	Vout                  uint32   `json:"vout"`
	Prevout               *Vout    `json:"prevout"`
	ScriptSig             string   `json:"scriptsig"`
	ScriptSigAsm          string   `json:"scriptsig_asm"`
	Witness               []string `json:"witness,omitempty"`
	IsCoinbase            bool     `json:"is_coinbase"`
	Sequence              uint32   `json:"sequence"`
	InnerWitnessScriptAsm string   `json:"inner_witnessscript_asm,omitempty"`
}

// Tx is a transaction in Esplora format.
type Tx struct {
	TxID     string `json:"txid"`
	Version  int32  `json:"version"`
	Locktime uint32 `json:"locktime"`
	Vin      []Vin  `json:"vin"`
	Vout     []Vout `json:"vout"`
	Size     int    `json:"size"`
	Weight   int    `json:"weight"`
	Fee      int64  `json:"fee"`
	Status   Status `json:"status"`
}

// UTXO is an entry of /address/{a}/utxo.
type UTXO struct {
	TxID   string `json:"txid"`
	Vout   uint32 `json:"vout"`
	Status Status `json:"status"`
	Value  int64  `json:"value"`
}

// Outspend is /tx/{txid}/outspend/{vout}.
type Outspend struct {
	Spent  bool    `json:"spent"`
	TxID   string  `json:"txid,omitempty"`
	Vin    *uint32 `json:"vin,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Stats are the chain_stats / mempool_stats of an address.
type Stats struct {
	FundedTxoCount int   `json:"funded_txo_count"`
	FundedTxoSum   int64 `json:"funded_txo_sum"`
	SpentTxoCount  int   `json:"spent_txo_count"`
	SpentTxoSum    int64 `json:"spent_txo_sum"`
	TxCount        int   `json:"tx_count"`
}

// AddressInfo is /address/{a}.
type AddressInfo struct {
	Address      string `json:"address"`
	ChainStats   Stats  `json:"chain_stats"`
	MempoolStats Stats  `json:"mempool_stats"`
}

func scriptType(script []byte) string {
	switch txscript.GetScriptClass(script) {
	case txscript.PubKeyTy:
		return "p2pk"
	case txscript.PubKeyHashTy:
		return "p2pkh"
	case txscript.ScriptHashTy:
		return "p2sh"
	case txscript.WitnessV0PubKeyHashTy:
		return "v0_p2wpkh"
	case txscript.WitnessV0ScriptHashTy:
		return "v0_p2wsh"
	case txscript.WitnessV1TaprootTy:
		return "v1_p2tr"
	case txscript.NullDataTy:
		return "op_return"
	case txscript.MultiSigTy:
		return "multisig"
	}
	return "unknown"
}

func asm(script []byte) string {
	s, _ := txscript.DisasmString(script)
	return s
}

func renderOut(o *output) *Vout {
	if o == nil {
		return nil
	}
	return &Vout{
		ScriptPubKey: hex.EncodeToString(o.script), ScriptPubKeyAsm: asm(o.script),
		ScriptPubKeyType: scriptType(o.script), ScriptPubKeyAddress: o.addr, Value: o.value,
	}
}

func status(e *txEntry) Status {
	if e.block == nil {
		return Status{}
	}
	return Status{Confirmed: true, BlockHeight: e.block.Height, BlockHash: e.block.Hash, BlockTime: e.block.Time}
}

// render converts an entry; callers hold mu.
func (ix *Index) render(e *txEntry) Tx {
	tx := e.tx
	t := Tx{
		TxID: e.txid, Version: tx.Version, Locktime: tx.LockTime,
		Size: tx.SerializeSize(), Weight: tx.SerializeSizeStripped()*3 + tx.SerializeSize(),
		Status: status(e),
	}
	coinbase := isCoinbaseTx(tx)
	var in, out int64
	for i, txin := range tx.TxIn {
		v := Vin{
			TxID: txin.PreviousOutPoint.Hash.String(), Vout: txin.PreviousOutPoint.Index,
			ScriptSig: hex.EncodeToString(txin.SignatureScript), Sequence: txin.Sequence, IsCoinbase: coinbase,
		}
		if !coinbase {
			v.ScriptSigAsm = asm(txin.SignatureScript)
			if p := e.prevouts[i]; p != nil {
				v.Prevout = renderOut(p)
				in += p.value
				if v.Prevout.ScriptPubKeyType == "v0_p2wsh" && len(txin.Witness) > 0 {
					v.InnerWitnessScriptAsm = asm(txin.Witness[len(txin.Witness)-1])
				}
			}
		}
		for _, w := range txin.Witness {
			v.Witness = append(v.Witness, hex.EncodeToString(w))
		}
		t.Vin = append(t.Vin, v)
	}
	for _, o := range tx.TxOut {
		t.Vout = append(t.Vout, *renderOut(ix.newOutput(o)))
		out += o.Value
	}
	if !coinbase && in >= out {
		t.Fee = in - out
	}
	return t
}

// Tx returns a transaction in Esplora format.
func (ix *Index) Tx(txid string) (Tx, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e := ix.lookup(txid)
	if e == nil {
		return Tx{}, false
	}
	return ix.render(e), true
}

// TxHex returns the serialized transaction.
func (ix *Index) TxHex(txid string) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e := ix.lookup(txid)
	if e == nil {
		return "", false
	}
	var buf bytes.Buffer
	_ = e.tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes()), true
}

// TxStatus returns the confirmation status of a transaction.
func (ix *Index) TxStatus(txid string) (Status, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e := ix.lookup(txid)
	if e == nil {
		return Status{}, false
	}
	return status(e), true
}

// Outspends returns the spend state of every output of a transaction.
func (ix *Index) Outspends(txid string) ([]Outspend, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e := ix.lookup(txid)
	if e == nil {
		return nil, false
	}
	hash := e.tx.TxHash()
	out := make([]Outspend, len(e.tx.TxOut))
	for i := range e.tx.TxOut {
		out[i] = ix.outspend(wire.OutPoint{Hash: hash, Index: uint32(i)})
	}
	return out, true
}

func (ix *Index) outspend(op wire.OutPoint) Outspend {
	s, ok := ix.spentBy(op)
	if !ok {
		return Outspend{}
	}
	vin := s.vin
	o := Outspend{Spent: true, TxID: s.txid, Vin: &vin}
	if e := ix.lookup(s.txid); e != nil {
		st := status(e)
		o.Status = &st
	}
	return o
}

// Outspend returns the spend state of one output; ok is false for an unknown transaction or output.
func (ix *Index) Outspend(txid string, vout uint32) (Outspend, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e := ix.lookup(txid)
	if e == nil || int(vout) >= len(e.tx.TxOut) {
		return Outspend{}, false
	}
	return ix.outspend(wire.OutPoint{Hash: e.tx.TxHash(), Index: vout}), true
}

// maxConfirmedTxs bounds /address/{a}/txs like Esplora's first page.
const maxConfirmedTxs = 50

// AddressTxs returns mempool transactions (newest first) and then confirmed ones (newest first).
func (ix *Index) AddressTxs(addr string) []Tx {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := []Tx{}
	for _, id := range ix.memAddr[addr] {
		if e := ix.mem[id]; e != nil {
			out = append(out, ix.render(e))
		}
	}
	list := ix.addrTxs[addr]
	for i, n := len(list)-1, 0; i >= 0 && n < maxConfirmedTxs; i, n = i-1, n+1 {
		out = append(out, ix.render(ix.txs[list[i]]))
	}
	return out
}

// UTXOs returns the unspent outputs of an address; outputs spent in the mempool are left out.
func (ix *Index) UTXOs(addr string) []UTXO {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := []UTXO{}
	collect := func(e *txEntry) {
		hash := e.tx.TxHash()
		for i, o := range e.tx.TxOut {
			op := wire.OutPoint{Hash: hash, Index: uint32(i)}
			if ix.scriptAddress(o.PkScript) != addr {
				continue
			}
			if _, spent := ix.spentBy(op); spent {
				continue
			}
			out = append(out, UTXO{TxID: e.txid, Vout: uint32(i), Status: status(e), Value: o.Value})
		}
	}
	for _, id := range ix.addrTxs[addr] {
		collect(ix.txs[id])
	}
	for _, id := range ix.memAddr[addr] {
		if e := ix.mem[id]; e != nil {
			collect(e)
		}
	}
	return out
}

// Address returns the chain and mempool statistics of an address.
func (ix *Index) Address(addr string) AddressInfo {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	stats := func(ids []string, get func(string) *txEntry) Stats {
		var s Stats
		for _, id := range ids {
			e := get(id)
			if e == nil {
				continue
			}
			s.TxCount++
			for _, p := range e.prevouts {
				if p != nil && p.addr == addr {
					s.SpentTxoCount++
					s.SpentTxoSum += p.value
				}
			}
			for _, o := range e.tx.TxOut {
				if ix.scriptAddress(o.PkScript) == addr {
					s.FundedTxoCount++
					s.FundedTxoSum += o.Value
				}
			}
		}
		return s
	}
	return AddressInfo{
		Address:      addr,
		ChainStats:   stats(ix.addrTxs[addr], func(id string) *txEntry { return ix.txs[id] }),
		MempoolStats: stats(ix.memAddr[addr], func(id string) *txEntry { return ix.mem[id] }),
	}
}
