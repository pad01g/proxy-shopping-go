// Package bitcoinrpc is a small bitcoind JSON-RPC client.
package bitcoinrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Client talks to one bitcoind endpoint (optionally a wallet endpoint).
type Client struct {
	endpoint string // without credentials
	user     string
	pass     string
	http     *http.Client
	id       *atomic.Int64
}

// RPCError is an error answer of bitcoind.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("bitcoind: %s (code %d)", e.Message, e.Code) }

// Well known error codes.
const (
	ErrCodeWalletNotFound      = -18
	ErrCodeWalletAlreadyLoaded = -35
	ErrCodeWalletExists        = -4
	ErrCodeInWarmup            = -28
	ErrCodeNotFound            = -5
)

// IsCode tells whether err is an RPC error with the given code.
func IsCode(err error, code int) bool {
	var re *RPCError
	return errors.As(err, &re) && re.Code == code
}

// New parses a URL like http://user:pass@host:port[/wallet/name].
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bitcoind url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("bitcoind url: unsupported scheme %q", u.Scheme)
	}
	c := &Client{http: &http.Client{Timeout: 60 * time.Second}, id: new(atomic.Int64)}
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
		u.User = nil
	}
	c.endpoint = strings.TrimRight(u.String(), "/")
	return c, nil
}

// Wallet returns a client for the wallet endpoint /wallet/<name>.
func (c *Client) Wallet(name string) *Client {
	base := c.endpoint
	if i := strings.Index(base, "/wallet/"); i >= 0 {
		base = base[:i]
	}
	w := *c
	w.endpoint = base + "/wallet/" + url.PathEscape(name)
	return &w
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Call runs a method and decodes its result into out (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(request{JSONRPC: "1.0", ID: c.id.Add(1), Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		// bitcoind answers 401/403 etc. without JSON
		return fmt.Errorf("%s: HTTP %d: %s", method, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if r.Error != nil {
		return r.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("%s result: %w", method, err)
	}
	return nil
}
