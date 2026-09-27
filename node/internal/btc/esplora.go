package btc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Esplora is a client of the subset of the Esplora HTTP API the nodes use.
type Esplora struct {
	Base string
	HTTP *http.Client
}

// NewEsplora returns a client for base (e.g. https://esplora.test).
func NewEsplora(base string, hc *http.Client) *Esplora {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Esplora{Base: strings.TrimRight(base, "/"), HTTP: hc}
}

// TxStatus is the confirmation state of a transaction.
type TxStatus struct {
	Confirmed   bool   `json:"confirmed"`
	BlockHeight int64  `json:"block_height,omitempty"`
	BlockHash   string `json:"block_hash,omitempty"`
	BlockTime   int64  `json:"block_time,omitempty"`
}

// UTXO is an unspent output of an address.
type UTXO struct {
	TxID   string   `json:"txid"`
	Vout   uint32   `json:"vout"`
	Value  int64    `json:"value"`
	Status TxStatus `json:"status"`
}

// Tx is the Esplora transaction format (the fields we use).
type Tx struct {
	TxID     string   `json:"txid"`
	Version  int32    `json:"version"`
	Locktime uint32   `json:"locktime"`
	Vin      []TxIn   `json:"vin"`
	Vout     []TxOut  `json:"vout"`
	Size     int      `json:"size"`
	Weight   int      `json:"weight"`
	Fee      int64    `json:"fee"`
	Status   TxStatus `json:"status"`
}

// TxIn is an input of Tx.
type TxIn struct {
	TxID         string   `json:"txid"`
	Vout         uint32   `json:"vout"`
	Prevout      *TxOut   `json:"prevout"`
	ScriptSig    string   `json:"scriptsig"`
	Witness      []string `json:"witness,omitempty"`
	IsCoinbase   bool     `json:"is_coinbase"`
	Sequence     uint32   `json:"sequence"`
	InnerWitness string   `json:"inner_witnessscript_asm,omitempty"`
}

// TxOut is an output of Tx.
type TxOut struct {
	ScriptPubKey        string `json:"scriptpubkey"`
	ScriptPubKeyType    string `json:"scriptpubkey_type,omitempty"`
	ScriptPubKeyAddress string `json:"scriptpubkey_address,omitempty"`
	Value               int64  `json:"value"`
}

// Outspend tells whether and by whom an output was spent.
type Outspend struct {
	Spent  bool      `json:"spent"`
	TxID   string    `json:"txid,omitempty"`
	Vin    uint32    `json:"vin,omitempty"`
	Status *TxStatus `json:"status,omitempty"`
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("esplora: HTTP %d: %s", e.Status, e.Body) }

// IsNotFound tells whether err is a 404 of Esplora.
func IsNotFound(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && he.Status == http.StatusNotFound
}

func (c *Esplora) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("esplora %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("esplora %s %s: %w", method, path, err)
	}
	if res.StatusCode/100 != 2 {
		return nil, &HTTPError{Status: res.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return data, nil
}

func (c *Esplora) getJSON(ctx context.Context, path string, v any) error {
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("esplora %s: %w", path, err)
	}
	return nil
}

// TipHeight is GET /blocks/tip/height.
func (c *Esplora) TipHeight(ctx context.Context) (int64, error) {
	data, err := c.do(ctx, http.MethodGet, "/blocks/tip/height", nil)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// AddressUTXOs is GET /address/{a}/utxo.
func (c *Esplora) AddressUTXOs(ctx context.Context, addr string) ([]UTXO, error) {
	var out []UTXO
	return out, c.getJSON(ctx, "/address/"+addr+"/utxo", &out)
}

// AddressTxs is GET /address/{a}/txs.
func (c *Esplora) AddressTxs(ctx context.Context, addr string) ([]Tx, error) {
	var out []Tx
	return out, c.getJSON(ctx, "/address/"+addr+"/txs", &out)
}

// Tx is GET /tx/{txid}.
func (c *Esplora) Tx(ctx context.Context, txid string) (*Tx, error) {
	var out Tx
	if err := c.getJSON(ctx, "/tx/"+txid, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TxHex is GET /tx/{txid}/hex.
func (c *Esplora) TxHex(ctx context.Context, txid string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/tx/"+txid+"/hex", nil)
	return strings.TrimSpace(string(data)), err
}

// TxStatus is GET /tx/{txid}/status.
func (c *Esplora) TxStatus(ctx context.Context, txid string) (*TxStatus, error) {
	var out TxStatus
	if err := c.getJSON(ctx, "/tx/"+txid+"/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Outspend is GET /tx/{txid}/outspend/{vout}.
func (c *Esplora) Outspend(ctx context.Context, txid string, vout uint32) (*Outspend, error) {
	var out Outspend
	if err := c.getJSON(ctx, fmt.Sprintf("/tx/%s/outspend/%d", txid, vout), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Broadcast is POST /tx; it returns the txid.
func (c *Esplora) Broadcast(ctx context.Context, txHex string) (string, error) {
	data, err := c.do(ctx, http.MethodPost, "/tx", bytes.NewBufferString(txHex))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// FeeEstimates is GET /fee-estimates (confirmation target → sat/vB).
func (c *Esplora) FeeEstimates(ctx context.Context) (map[string]float64, error) {
	out := map[string]float64{}
	return out, c.getJSON(ctx, "/fee-estimates", &out)
}

// Confirmations returns the number of confirmations of a transaction (0 while in the mempool).
func (c *Esplora) Confirmations(ctx context.Context, txid string) (int64, error) {
	st, err := c.TxStatus(ctx, txid)
	if err != nil {
		return 0, err
	}
	if !st.Confirmed {
		return 0, nil
	}
	tip, err := c.TipHeight(ctx)
	if err != nil {
		return 0, err
	}
	return tip - st.BlockHeight + 1, nil
}
