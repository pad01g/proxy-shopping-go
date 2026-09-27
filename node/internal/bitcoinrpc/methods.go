package bitcoinrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GetBlockCount returns the height of the tip.
func (c *Client) GetBlockCount(ctx context.Context) (int64, error) {
	var n int64
	return n, c.Call(ctx, "getblockcount", nil, &n)
}

// GetBlockHash returns the hash of the block at height.
func (c *Client) GetBlockHash(ctx context.Context, height int64) (string, error) {
	var h string
	return h, c.Call(ctx, "getblockhash", []any{height}, &h)
}

// BlockHeader is the header part of getblock.
type BlockHeader struct {
	Hash              string `json:"hash"`
	Height            int64  `json:"height"`
	Time              int64  `json:"time"`
	PreviousBlockHash string `json:"previousblockhash"`
	Confirmations     int64  `json:"confirmations"`
}

// BlockTx is a transaction of getblock verbosity 2 (only the fields we need).
type BlockTx struct {
	TxID string `json:"txid"`
	Hex  string `json:"hex"`
}

// Block is getblock with verbosity 2.
type Block struct {
	BlockHeader
	Tx []BlockTx `json:"tx"`
}

// GetBlock returns a block with its transactions (verbosity 2).
func (c *Client) GetBlock(ctx context.Context, hash string) (*Block, error) {
	var b Block
	if err := c.Call(ctx, "getblock", []any{hash, 2}, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// GetBlockHeader returns the header of a block.
func (c *Client) GetBlockHeader(ctx context.Context, hash string) (*BlockHeader, error) {
	var h BlockHeader
	if err := c.Call(ctx, "getblockheader", []any{hash, true}, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// RawTx is getrawtransaction verbose (the fields we need).
type RawTx struct {
	TxID          string `json:"txid"`
	Hex           string `json:"hex"`
	BlockHash     string `json:"blockhash"`
	Confirmations int64  `json:"confirmations"`
	BlockTime     int64  `json:"blocktime"`
}

// GetRawTransactionHex returns the serialized transaction.
func (c *Client) GetRawTransactionHex(ctx context.Context, txid string) (string, error) {
	var h string
	return h, c.Call(ctx, "getrawtransaction", []any{txid, false}, &h)
}

// GetRawTransaction returns the verbose form of a transaction.
func (c *Client) GetRawTransaction(ctx context.Context, txid string) (*RawTx, error) {
	var t RawTx
	if err := c.Call(ctx, "getrawtransaction", []any{txid, true}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetRawMempool returns the txids in the mempool.
func (c *Client) GetRawMempool(ctx context.Context) ([]string, error) {
	var ids []string
	return ids, c.Call(ctx, "getrawmempool", nil, &ids)
}

// MempoolEntry is getmempoolentry (the fields we need).
type MempoolEntry struct {
	VSize int64 `json:"vsize"`
	Time  int64 `json:"time"`
	Fees  struct {
		Base float64 `json:"base"`
	} `json:"fees"`
}

// GetMempoolEntry returns the mempool data of a transaction.
func (c *Client) GetMempoolEntry(ctx context.Context, txid string) (*MempoolEntry, error) {
	var e MempoolEntry
	if err := c.Call(ctx, "getmempoolentry", []any{txid}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// SendRawTransaction broadcasts a transaction and returns its txid.
func (c *Client) SendRawTransaction(ctx context.Context, hex string) (string, error) {
	var id string
	return id, c.Call(ctx, "sendrawtransaction", []any{hex}, &id)
}

// MempoolAcceptResult is one entry of testmempoolaccept.
type MempoolAcceptResult struct {
	TxID         string `json:"txid"`
	Allowed      bool   `json:"allowed"`
	RejectReason string `json:"reject-reason"`
}

// TestMempoolAccept checks transactions without broadcasting them.
func (c *Client) TestMempoolAccept(ctx context.Context, hexes ...string) ([]MempoolAcceptResult, error) {
	var out []MempoolAcceptResult
	return out, c.Call(ctx, "testmempoolaccept", []any{hexes}, &out)
}

// GetNewAddress returns a new bech32 address of the wallet.
func (c *Client) GetNewAddress(ctx context.Context) (string, error) {
	var a string
	return a, c.Call(ctx, "getnewaddress", []any{"", "bech32"}, &a)
}

// GenerateToAddress mines n blocks paying to addr and returns their hashes. maxtries of generatetoaddress
// counts nonces over all blocks of a call, and a signet block needs a few million, so it mines in small
// batches until n blocks exist.
func (c *Client) GenerateToAddress(ctx context.Context, n int, addr string) ([]string, error) {
	var all []string
	for len(all) < n {
		var hs []string
		if err := c.Call(ctx, "generatetoaddress", []any{min(n-len(all), 10), addr, 1_000_000_000}, &hs); err != nil {
			return all, err
		}
		all = append(all, hs...)
	}
	return all, nil
}

// SendToAddress pays amount (BTC as a decimal string such as "0.00100000") from the wallet.
func (c *Client) SendToAddress(ctx context.Context, addr, amount string) (string, error) {
	var id string
	return id, c.Call(ctx, "sendtoaddress", []any{addr, json.Number(amount)}, &id)
}

// GetBalance returns the trusted wallet balance in BTC.
func (c *Client) GetBalance(ctx context.Context) (float64, error) {
	var b float64
	return b, c.Call(ctx, "getbalance", nil, &b)
}

// CreateWallet creates a descriptor wallet; an existing wallet is not an error.
func (c *Client) CreateWallet(ctx context.Context, name string) error {
	err := c.Call(ctx, "createwallet", []any{name}, nil)
	if err != nil && (IsCode(err, ErrCodeWalletExists) || IsCode(err, ErrCodeWalletAlreadyLoaded) || isAlready(err)) {
		return nil
	}
	return err
}

// LoadWallet loads a wallet; an already loaded wallet is not an error.
func (c *Client) LoadWallet(ctx context.Context, name string) error {
	err := c.Call(ctx, "loadwallet", []any{name}, nil)
	if err != nil && (IsCode(err, ErrCodeWalletAlreadyLoaded) || isAlready(err)) {
		return nil
	}
	return err
}

// EnsureWallet loads the wallet or creates it when it does not exist.
func (c *Client) EnsureWallet(ctx context.Context, name string) error {
	err := c.LoadWallet(ctx, name)
	if err == nil {
		return nil
	}
	if IsCode(err, ErrCodeWalletNotFound) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "does not exist") {
		if err := c.CreateWallet(ctx, name); err != nil {
			return fmt.Errorf("create wallet %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("load wallet %s: %w", name, err)
}

func isAlready(err error) bool {
	s := err.Error()
	return strings.Contains(s, "already exists") || strings.Contains(s, "already loaded")
}

// SmartFee is estimatesmartfee.
type SmartFee struct {
	FeeRate float64  `json:"feerate"` // BTC/kvB
	Errors  []string `json:"errors"`
	Blocks  int      `json:"blocks"`
}

// EstimateSmartFee estimates the fee rate for a confirmation target.
func (c *Client) EstimateSmartFee(ctx context.Context, target int) (*SmartFee, error) {
	var f SmartFee
	if err := c.Call(ctx, "estimatesmartfee", []any{target}, &f); err != nil {
		return nil, err
	}
	return &f, nil
}
