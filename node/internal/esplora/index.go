// Package esplora serves the subset of the Esplora HTTP API the lab uses (esplora-lite), backed by bitcoind.
// It keeps an in-memory index of every block since genesis plus the mempool, which suits small chains such as
// the lab signet.
package esplora

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
)

// Source is the part of bitcoind the index needs.
type Source interface {
	GetBlockCount(ctx context.Context) (int64, error)
	GetBlockHash(ctx context.Context, height int64) (string, error)
	GetBlock(ctx context.Context, hash string) (*bitcoinrpc.Block, error)
	GetRawMempool(ctx context.Context) ([]string, error)
	GetRawTransactionHex(ctx context.Context, txid string) (string, error)
	SendRawTransaction(ctx context.Context, hex string) (string, error)
	EstimateSmartFee(ctx context.Context, target int) (*bitcoinrpc.SmartFee, error)
}

type blockRef struct {
	Hash   string
	Height int64
	Time   int64
	TxIDs  []string
}

type output struct {
	value  int64
	script []byte
	addr   string // empty if the script has no address
}

type spend struct {
	txid string
	vin  uint32
}

type txEntry struct {
	txid     string
	tx       *wire.MsgTx
	block    *blockRef // nil while in the mempool
	seen     int64     // first seen in the mempool
	prevouts []*output // nil entries for coinbase inputs or unknown prevouts
}

// Index is the address and spend index.
type Index struct {
	src    Source
	params *chaincfg.Params
	log    *slog.Logger

	syncMu sync.Mutex // serializes Sync and RefreshMempool

	mu      sync.RWMutex
	blocks  []*blockRef // by height
	txs     map[string]*txEntry
	outs    map[wire.OutPoint]*output
	spends  map[wire.OutPoint]spend
	addrTxs map[string][]string // confirmed, in chain order

	mem       map[string]*txEntry
	memOuts   map[wire.OutPoint]*output
	memSpends map[wire.OutPoint]spend
	memAddr   map[string][]string
}

// NewIndex returns an empty index; call Sync to fill it.
func NewIndex(src Source, params *chaincfg.Params, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	return &Index{
		src: src, params: params, log: log,
		txs: map[string]*txEntry{}, outs: map[wire.OutPoint]*output{}, spends: map[wire.OutPoint]spend{},
		addrTxs: map[string][]string{},
		mem:     map[string]*txEntry{}, memOuts: map[wire.OutPoint]*output{}, memSpends: map[wire.OutPoint]spend{},
		memAddr: map[string][]string{},
	}
}

// errNotChild reports a block whose parent is not the indexed tip: the node reorganized while we read it.
var errNotChild = errors.New("block does not extend the indexed tip")

// maxReorgRetries bounds how often one Sync starts over after the chain changed under it.
const maxReorgRetries = 10

// Sync follows the chain to the node's tip (undoing reorganized blocks) and refreshes the mempool.
func (ix *Index) Sync(ctx context.Context) error {
	ix.syncMu.Lock()
	defer ix.syncMu.Unlock()
	for attempt := 0; ; attempt++ {
		err := ix.syncBlocks(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, errNotChild) || attempt >= maxReorgRetries {
			return err
		}
		ix.log.Info("chain changed while syncing, starting over", "err", err)
	}
	return ix.refreshMempoolLocked(ctx)
}

// syncBlocks rolls back to the fork point with the node's best chain and indexes the blocks after it. Every
// block must name the indexed tip as its parent; when it does not (a reorg between our calls), errNotChild.
func (ix *Index) syncBlocks(ctx context.Context) error {
	count, err := ix.src.GetBlockCount(ctx)
	if err != nil {
		return fmt.Errorf("block count: %w", err)
	}
	// undo blocks that are no longer on the best chain
	for {
		top := ix.top()
		if top == nil {
			break
		}
		if top.Height <= count {
			h, err := ix.src.GetBlockHash(ctx, top.Height)
			if err != nil {
				return fmt.Errorf("block hash %d: %w", top.Height, err)
			}
			if h == top.Hash {
				break
			}
		}
		ix.log.Info("reorg: undoing block", "height", top.Height, "hash", top.Hash)
		ix.undoTop()
	}
	for h := ix.height() + 1; h <= count; h++ {
		hash, err := ix.src.GetBlockHash(ctx, h)
		if err != nil {
			return fmt.Errorf("block hash %d: %w", h, err)
		}
		blk, err := ix.src.GetBlock(ctx, hash)
		if err != nil {
			return fmt.Errorf("block %s: %w", hash, err)
		}
		if err := ix.addBlock(blk); err != nil {
			return fmt.Errorf("index block %d: %w", h, err)
		}
	}
	return nil
}

// RefreshMempool re-reads the mempool only (e.g. right after a broadcast).
func (ix *Index) RefreshMempool(ctx context.Context) error {
	ix.syncMu.Lock()
	defer ix.syncMu.Unlock()
	return ix.refreshMempoolLocked(ctx)
}

func (ix *Index) top() *blockRef {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.blocks) == 0 {
		return nil
	}
	return ix.blocks[len(ix.blocks)-1]
}

func (ix *Index) height() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return int64(len(ix.blocks)) - 1
}

// Tip returns the height and hash of the indexed tip (-1 when empty).
func (ix *Index) Tip() (int64, string) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if len(ix.blocks) == 0 {
		return -1, ""
	}
	b := ix.blocks[len(ix.blocks)-1]
	return b.Height, b.Hash
}

// BlockHash returns the hash of the indexed block at a height.
func (ix *Index) BlockHash(height int64) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if height < 0 || height >= int64(len(ix.blocks)) {
		return "", false
	}
	return ix.blocks[height].Hash, true
}

func decodeTx(h string) (*wire.MsgTx, error) {
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return &tx, nil
}

// scriptAddress returns the address of an output script, if it has one.
func (ix *Index) scriptAddress(script []byte) string {
	class, addrs, _, err := txscript.ExtractPkScriptAddrs(script, ix.params)
	if err != nil || len(addrs) != 1 {
		return ""
	}
	switch class {
	case txscript.PubKeyHashTy, txscript.ScriptHashTy, txscript.WitnessV0PubKeyHashTy,
		txscript.WitnessV0ScriptHashTy, txscript.WitnessV1TaprootTy:
		return addrs[0].EncodeAddress()
	}
	return ""
}

func (ix *Index) newOutput(o *wire.TxOut) *output {
	return &output{value: o.Value, script: o.PkScript, addr: ix.scriptAddress(o.PkScript)}
}

// txAddresses lists the distinct addresses a transaction pays or spends from.
func txAddresses(e *txEntry, outs []*output) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, p := range e.prevouts {
		if p != nil {
			add(p.addr)
		}
	}
	for _, o := range outs {
		add(o.addr)
	}
	return out
}

func (ix *Index) addBlock(blk *bitcoinrpc.Block) error {
	ref := &blockRef{Hash: blk.Hash, Height: blk.Height, Time: blk.Time}
	entries := make([]*txEntry, 0, len(blk.Tx))
	for _, t := range blk.Tx {
		tx, err := decodeTx(t.Hex)
		if err != nil {
			return fmt.Errorf("decode tx %s: %w", t.TxID, err)
		}
		entries = append(entries, &txEntry{txid: tx.TxHash().String(), tx: tx, block: ref})
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if int64(len(ix.blocks)) != blk.Height {
		return fmt.Errorf("block %d does not follow the indexed tip %d", blk.Height, len(ix.blocks)-1)
	}
	if n := len(ix.blocks); n > 0 && blk.PreviousBlockHash != ix.blocks[n-1].Hash {
		return fmt.Errorf("%w: block %d %s has parent %s, the indexed tip is %s", errNotChild, blk.Height, blk.Hash, blk.PreviousBlockHash, ix.blocks[n-1].Hash)
	}
	for _, e := range entries {
		e.prevouts = make([]*output, len(e.tx.TxIn))
		isCoinbase := isCoinbaseTx(e.tx)
		for i, in := range e.tx.TxIn {
			if isCoinbase {
				continue
			}
			e.prevouts[i] = ix.outs[in.PreviousOutPoint]
			if e.prevouts[i] == nil {
				ix.log.Warn("unknown prevout", "tx", e.txid, "prevout", in.PreviousOutPoint)
			}
			ix.spends[in.PreviousOutPoint] = spend{txid: e.txid, vin: uint32(i)}
		}
		outs := make([]*output, len(e.tx.TxOut))
		hash := e.tx.TxHash()
		for i, o := range e.tx.TxOut {
			outs[i] = ix.newOutput(o)
			ix.outs[wire.OutPoint{Hash: hash, Index: uint32(i)}] = outs[i]
		}
		for _, a := range txAddresses(e, outs) {
			ix.addrTxs[a] = append(ix.addrTxs[a], e.txid)
		}
		ix.txs[e.txid] = e
		ref.TxIDs = append(ref.TxIDs, e.txid)
	}
	ix.blocks = append(ix.blocks, ref)
	return nil
}

func (ix *Index) undoTop() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	b := ix.blocks[len(ix.blocks)-1]
	for i := len(b.TxIDs) - 1; i >= 0; i-- {
		e := ix.txs[b.TxIDs[i]]
		if e == nil {
			continue
		}
		var outs []*output
		hash := e.tx.TxHash()
		for vout := range e.tx.TxOut {
			op := wire.OutPoint{Hash: hash, Index: uint32(vout)}
			outs = append(outs, ix.outs[op])
			delete(ix.outs, op)
		}
		if !isCoinbaseTx(e.tx) {
			for _, in := range e.tx.TxIn {
				delete(ix.spends, in.PreviousOutPoint)
			}
		}
		for _, a := range txAddresses(e, outs) {
			ix.addrTxs[a] = removeLast(ix.addrTxs[a], e.txid)
			if len(ix.addrTxs[a]) == 0 {
				delete(ix.addrTxs, a)
			}
		}
		delete(ix.txs, e.txid)
	}
	ix.blocks = ix.blocks[:len(ix.blocks)-1]
}

func removeLast(list []string, id string) []string {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func isCoinbaseTx(tx *wire.MsgTx) bool {
	if len(tx.TxIn) != 1 {
		return false
	}
	p := tx.TxIn[0].PreviousOutPoint
	return p.Index == wire.MaxPrevOutIndex && p.Hash == (chainhash.Hash{})
}

func (ix *Index) refreshMempoolLocked(ctx context.Context) error {
	ids, err := ix.src.GetRawMempool(ctx)
	if err != nil {
		return fmt.Errorf("mempool: %w", err)
	}
	ix.mu.RLock()
	old := ix.mem
	ix.mu.RUnlock()

	now := time.Now().Unix()
	next := make(map[string]*txEntry, len(ids))
	for _, id := range ids {
		if e, ok := old[id]; ok {
			next[id] = e
			continue
		}
		ix.mu.RLock()
		_, confirmed := ix.txs[id]
		ix.mu.RUnlock()
		if confirmed {
			continue
		}
		h, err := ix.src.GetRawTransactionHex(ctx, id)
		if err != nil {
			// evicted or mined since getrawmempool; the next poll sees it
			ix.log.Debug("mempool tx vanished", "txid", id, "err", err)
			continue
		}
		tx, err := decodeTx(h)
		if err != nil {
			return fmt.Errorf("decode mempool tx %s: %w", id, err)
		}
		next[id] = &txEntry{txid: id, tx: tx, seen: now}
	}

	memOuts := map[wire.OutPoint]*output{}
	for _, e := range next {
		hash := e.tx.TxHash()
		for i, o := range e.tx.TxOut {
			memOuts[wire.OutPoint{Hash: hash, Index: uint32(i)}] = ix.newOutput(o)
		}
	}
	memSpends := map[wire.OutPoint]spend{}
	memAddr := map[string][]string{}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, e := range next {
		e.prevouts = make([]*output, len(e.tx.TxIn))
		for i, in := range e.tx.TxIn {
			p := ix.outs[in.PreviousOutPoint]
			if p == nil {
				p = memOuts[in.PreviousOutPoint]
			}
			e.prevouts[i] = p
			memSpends[in.PreviousOutPoint] = spend{txid: e.txid, vin: uint32(i)}
		}
		hash := e.tx.TxHash()
		var outs []*output
		for i := range e.tx.TxOut {
			outs = append(outs, memOuts[wire.OutPoint{Hash: hash, Index: uint32(i)}])
		}
		for _, a := range txAddresses(e, outs) {
			memAddr[a] = append(memAddr[a], e.txid)
		}
	}
	for a, list := range memAddr {
		sort.Slice(list, func(i, j int) bool {
			if next[list[i]].seen != next[list[j]].seen {
				return next[list[i]].seen > next[list[j]].seen
			}
			return list[i] < list[j]
		})
		memAddr[a] = list
	}
	ix.mem, ix.memOuts, ix.memSpends, ix.memAddr = next, memOuts, memSpends, memAddr
	return nil
}

// Run syncs on a ticker until ctx ends.
func (ix *Index) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := ix.Sync(ctx); err != nil && ctx.Err() == nil {
			ix.log.Warn("sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// lookup returns a transaction, confirmed or in the mempool. Callers hold mu.
func (ix *Index) lookup(txid string) *txEntry {
	if e := ix.txs[txid]; e != nil {
		return e
	}
	return ix.mem[txid]
}

// spentBy returns the spender of an outpoint, preferring a confirmed one. Callers hold mu.
func (ix *Index) spentBy(op wire.OutPoint) (spend, bool) {
	if s, ok := ix.spends[op]; ok {
		return s, true
	}
	s, ok := ix.memSpends[op]
	return s, ok
}
