package esplora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
)

// feeTargets are the confirmation targets of /fee-estimates (as in Esplora).
var feeTargets = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 144, 504, 1008}

// Server is the HTTP API.
type Server struct {
	ix  *Index
	log *slog.Logger
	mux *http.ServeMux

	feeMu   sync.Mutex
	fees    map[string]float64
	feesAt  time.Time
	feesTTL time.Duration
}

// NewServer builds the handlers on top of an index.
func NewServer(ix *Index, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{ix: ix, log: log, mux: http.NewServeMux(), feesTTL: 30 * time.Second}
	s.mux.HandleFunc("GET /blocks/tip/height", s.tipHeight)
	s.mux.HandleFunc("GET /blocks/tip/hash", s.tipHash)
	s.mux.HandleFunc("GET /block-height/{h}", s.blockHeight)
	s.mux.HandleFunc("GET /address/{a}", s.address)
	s.mux.HandleFunc("GET /address/{a}/utxo", s.addressUTXO)
	s.mux.HandleFunc("GET /address/{a}/txs", s.addressTxs)
	s.mux.HandleFunc("GET /tx/{txid}", s.tx)
	s.mux.HandleFunc("GET /tx/{txid}/hex", s.txHex)
	s.mux.HandleFunc("GET /tx/{txid}/status", s.txStatus)
	s.mux.HandleFunc("GET /tx/{txid}/outspend/{vout}", s.outspend)
	s.mux.HandleFunc("GET /tx/{txid}/outspends", s.outspends)
	s.mux.HandleFunc("POST /tx", s.broadcast)
	s.mux.HandleFunc("GET /fee-estimates", s.feeEstimates)
	return s
}

// ServeHTTP adds CORS for browsers and answers preflight requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, text)
}

func (s *Server) tipHeight(w http.ResponseWriter, _ *http.Request) {
	h, _ := s.ix.Tip()
	if h < 0 {
		writeText(w, http.StatusServiceUnavailable, "not synced")
		return
	}
	writeText(w, http.StatusOK, strconv.FormatInt(h, 10))
}

func (s *Server) tipHash(w http.ResponseWriter, _ *http.Request) {
	h, hash := s.ix.Tip()
	if h < 0 {
		writeText(w, http.StatusServiceUnavailable, "not synced")
		return
	}
	writeText(w, http.StatusOK, hash)
}

func (s *Server) blockHeight(w http.ResponseWriter, r *http.Request) {
	h, err := strconv.ParseInt(r.PathValue("h"), 10, 64)
	if err != nil {
		writeText(w, http.StatusBadRequest, "invalid block height")
		return
	}
	hash, ok := s.ix.BlockHash(h)
	if !ok {
		writeText(w, http.StatusNotFound, "Block not found")
		return
	}
	writeText(w, http.StatusOK, hash)
}

// addr normalizes an address of the index network or answers 400.
func (s *Server) addr(w http.ResponseWriter, r *http.Request) (string, bool) {
	a, err := btcutil.DecodeAddress(r.PathValue("a"), s.ix.params)
	if err != nil || !a.IsForNet(s.ix.params) {
		writeText(w, http.StatusBadRequest, "Invalid Bitcoin address")
		return "", false
	}
	return a.EncodeAddress(), true
}

func (s *Server) address(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.addr(w, r); ok {
		writeJSON(w, s.ix.Address(a))
	}
}

func (s *Server) addressUTXO(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.addr(w, r); ok {
		writeJSON(w, s.ix.UTXOs(a))
	}
}

func (s *Server) addressTxs(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.addr(w, r); ok {
		writeJSON(w, s.ix.AddressTxs(a))
	}
}

const txNotFound = "Transaction not found"

func (s *Server) tx(w http.ResponseWriter, r *http.Request) {
	t, ok := s.ix.Tx(strings.ToLower(r.PathValue("txid")))
	if !ok {
		writeText(w, http.StatusNotFound, txNotFound)
		return
	}
	writeJSON(w, t)
}

func (s *Server) txHex(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ix.TxHex(strings.ToLower(r.PathValue("txid")))
	if !ok {
		writeText(w, http.StatusNotFound, txNotFound)
		return
	}
	writeText(w, http.StatusOK, h)
}

func (s *Server) txStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.ix.TxStatus(strings.ToLower(r.PathValue("txid")))
	if !ok {
		writeText(w, http.StatusNotFound, txNotFound)
		return
	}
	writeJSON(w, st)
}

func (s *Server) outspend(w http.ResponseWriter, r *http.Request) {
	vout, err := strconv.ParseUint(r.PathValue("vout"), 10, 32)
	if err != nil {
		writeText(w, http.StatusBadRequest, "invalid vout")
		return
	}
	o, ok := s.ix.Outspend(strings.ToLower(r.PathValue("txid")), uint32(vout))
	if !ok {
		writeText(w, http.StatusNotFound, txNotFound)
		return
	}
	writeJSON(w, o)
}

func (s *Server) outspends(w http.ResponseWriter, r *http.Request) {
	o, ok := s.ix.Outspends(strings.ToLower(r.PathValue("txid")))
	if !ok {
		writeText(w, http.StatusNotFound, txNotFound)
		return
	}
	writeJSON(w, o)
}

func (s *Server) broadcast(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeText(w, http.StatusBadRequest, "cannot read body")
		return
	}
	txHex := strings.TrimSpace(string(body))
	if _, err := decodeTx(txHex); err != nil {
		writeText(w, http.StatusBadRequest, "sendrawtransaction RPC error: invalid transaction hex")
		return
	}
	txid, err := s.ix.src.SendRawTransaction(r.Context(), txHex)
	if err != nil {
		var re *bitcoinrpc.RPCError
		if errors.As(err, &re) {
			data, _ := json.Marshal(re)
			writeText(w, http.StatusBadRequest, "sendrawtransaction RPC error: "+string(data))
			return
		}
		s.log.Warn("broadcast failed", "err", err)
		writeText(w, http.StatusBadGateway, "bitcoind unavailable")
		return
	}
	// make the transaction visible to the next query right away
	if err := s.ix.RefreshMempool(r.Context()); err != nil {
		s.log.Warn("mempool refresh after broadcast", "err", err)
	}
	writeText(w, http.StatusOK, txid)
}

func (s *Server) feeEstimates(w http.ResponseWriter, r *http.Request) {
	fees, err := s.estimates(r.Context())
	if err != nil {
		s.log.Warn("fee estimates", "err", err)
		writeText(w, http.StatusBadGateway, "bitcoind unavailable")
		return
	}
	writeJSON(w, fees)
}

// estimates asks bitcoind (cached); targets without an estimate get 1 sat/vB, the relay minimum.
func (s *Server) estimates(ctx context.Context) (map[string]float64, error) {
	s.feeMu.Lock()
	defer s.feeMu.Unlock()
	if s.fees != nil && time.Since(s.feesAt) < s.feesTTL {
		return s.fees, nil
	}
	fees := make(map[string]float64, len(feeTargets))
	for _, t := range feeTargets {
		f, err := s.ix.src.EstimateSmartFee(ctx, t)
		if err != nil {
			var re *bitcoinrpc.RPCError
			if !errors.As(err, &re) {
				return nil, fmt.Errorf("estimatesmartfee %d: %w", t, err)
			}
			f = &bitcoinrpc.SmartFee{}
		}
		rate := 1.0
		if f.FeeRate > 0 {
			rate = f.FeeRate * 1e5 // BTC/kvB → sat/vB
		}
		fees[strconv.Itoa(t)] = rate
	}
	s.fees, s.feesAt = fees, time.Now()
	return fees, nil
}
