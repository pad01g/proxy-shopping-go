package bitcoinrpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fake(t *testing.T, handle func(path, method string, params []json.RawMessage) (any, *RPCError)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "lab" || p != "pw" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, rerr := handle(r.URL.Path, req.Method, req.Params)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": req.ID, "result": res, "error": rerr})
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "http://lab:pw@", 1)
}

func TestCallsAndErrors(t *testing.T) {
	var lastPath string
	url := fake(t, func(path, method string, params []json.RawMessage) (any, *RPCError) {
		lastPath = path
		switch method {
		case "getblockcount":
			return 42, nil
		case "sendtoaddress":
			if string(params[1]) != "0.00001000" {
				return nil, &RPCError{Code: -3, Message: "amount " + string(params[1])}
			}
			return "txid", nil
		case "loadwallet":
			return nil, &RPCError{Code: ErrCodeWalletNotFound, Message: "Wallet file not found"}
		case "createwallet":
			return map[string]string{"name": "w"}, nil
		}
		return nil, &RPCError{Code: -32601, Message: "Method not found"}
	})
	c, err := New(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if n, err := c.GetBlockCount(ctx); err != nil || n != 42 {
		t.Fatalf("count %d %v", n, err)
	}
	w := c.Wallet("faucet")
	if id, err := w.SendToAddress(ctx, "tb1q", "0.00001000"); err != nil || id != "txid" {
		t.Fatalf("send %q %v", id, err)
	}
	if lastPath != "/wallet/faucet" {
		t.Fatalf("wallet path %q", lastPath)
	}
	if w.Wallet("other").endpoint != c.endpoint+"/wallet/other" {
		t.Fatal("wallet of a wallet client")
	}
	if err := c.EnsureWallet(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetRawMempool(ctx)
	if !IsCode(err, -32601) {
		t.Fatalf("rpc error not typed: %v", err)
	}

	bad, _ := New(strings.Replace(url, "lab:pw", "lab:wrong", 1))
	if _, err := bad.GetBlockCount(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("auth error: %v", err)
	}
	if _, err := New("ftp://x"); err == nil {
		t.Fatal("bad scheme accepted")
	}
}
