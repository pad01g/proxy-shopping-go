//go:build integration

// Package e2e runs whole order flows in-process: two psrelays, a real signet bitcoind behind esplora-lite, a
// shopper node and an escrow node (psnode), a fake shop and shopper-bot, and a scripted user.
package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/esplora"
	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/node"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

var keyDir = filepath.Join("..", "..", "..", "lab", "keys")

func labKeys(t *testing.T, name string) *keys.Set {
	t.Helper()
	s, err := keys.LoadMnemonicFile(filepath.Join(keyDir, name+".mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// chain is bitcoind with a funded faucet wallet, esplora-lite in front of it and auto-mining.
type chain struct {
	rpc     *bitcoinrpc.Client
	faucet  *faucet.Faucet
	esplora string
}

func startChain(t *testing.T, ctx context.Context) *chain {
	t.Helper()
	url := testutil.StartBitcoind(t)
	rpc, err := bitcoinrpc.New(url)
	if err != nil {
		t.Fatal(err)
	}
	f := faucet.New(faucet.Config{BTC: rpc, Wallet: "faucet", Log: testutil.Logger(t)})
	if err := f.Init(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	ix := esplora.NewIndex(rpc, keys.BTCParams, testutil.Logger(t))
	if err := ix.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	go ix.Run(ctx, 300*time.Millisecond)
	go f.AutoMine(ctx, time.Second)
	hs := httptest.NewServer(esplora.NewServer(ix, testutil.Logger(t)))
	t.Cleanup(hs.Close)
	return &chain{rpc: rpc, faucet: f, esplora: hs.URL}
}

// fund pays the escrow address and the escrow fee in one transaction (as the user's wallet would).
func (c *chain) fund(t *testing.T, ctx context.Context, lockAddr string, lock int64, feeAddr string, fee int64) (string, uint32) {
	t.Helper()
	var txid string
	outs := map[string]string{lockAddr: faucet.SatsToBTC(lock), feeAddr: faucet.SatsToBTC(fee)}
	if err := c.rpc.Wallet("faucet").Call(ctx, "sendmany", []any{"", outs}, &txid); err != nil {
		t.Fatal(err)
	}
	raw, err := c.rpc.GetRawTransactionHex(ctx, txid)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := hex.DecodeString(raw)
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	want, _ := btc.PkScript(lockAddr)
	for i, o := range tx.TxOut {
		if bytes.Equal(o.PkScript, want) {
			return txid, uint32(i)
		}
	}
	t.Fatal("funding without escrow output")
	return "", 0
}

// fakeShop is an HTTPS shop with a known payment gateway and the lab catalog of safe-shop.test.
func fakeShop(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><head><meta name="ps-payment-gateway" content="cardgw.test"><title>shop</title></head></html>`)
	})
	mux.HandleFunc("GET /api/products", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"shop":"safe-shop.test","currency":"JPY","shipping":{"amount":"800","currency":"JPY"},
"products":[{"sku":"A-100","name":"抹茶ティーセット","price":{"amount":"3200","currency":"JPY"}},{"sku":"A-200","name":"急須","price":{"amount":"12000","currency":"JPY"}}]}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

// fakeBot buys everything and reports shipped on the first tracking call, delivered on the second.
type fakeBot struct {
	mu        sync.Mutex
	purchases map[string]proto.PurchaseRequest
	tracked   map[string]int
	holdBack  map[string]bool // shop orders that stay "processing"
	dup       bool            // a request_id was bought twice, or was not the order id
}

func startBot(t *testing.T) (*fakeBot, string) {
	b := &fakeBot{purchases: map[string]proto.PurchaseRequest{}, tracked: map[string]int{}, holdBack: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/purchase", func(w http.ResponseWriter, r *http.Request) {
		var req proto.PurchaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.mu.Lock()
		// like shopper-bot, one purchase per request_id (§9); the node must always use the order id
		if _, again := b.purchases[req.RequestID]; again || req.RequestID != req.OrderID {
			b.dup = true
		}
		b.purchases[req.OrderID] = req
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(proto.PurchaseResult{
			RequestID: req.RequestID, Status: "ok", ShopOrderID: "SS-" + req.OrderID[:8],
			Total:    &proto.Money{Amount: "4000", Currency: "JPY"},
			Evidence: []proto.Evidence{{Kind: "json", SHA256: strings.Repeat("0", 64), MIME: "application/json"}},
		})
	})
	mux.HandleFunc("POST /v1/tracking", func(w http.ResponseWriter, r *http.Request) {
		var q proto.TrackingQuery
		_ = json.NewDecoder(r.Body).Decode(&q)
		b.mu.Lock()
		b.tracked[q.ShopOrderID]++
		n, hold := b.tracked[q.ShopOrderID], b.holdBack[q.ShopOrderID]
		b.mu.Unlock()
		status := "delivered"
		switch {
		case hold:
			status = "processing"
		case n == 1:
			status = "shipped"
		}
		_ = json.NewEncoder(w).Encode(proto.TrackingStatus{Status: status, Carrier: "Yamato", TrackingNo: "YT-1",
			UpdatedAt: json.RawMessage(fmt.Sprint(time.Now().Unix())), Evidence: []proto.Evidence{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return b, srv.URL
}

func (b *fakeBot) duplicated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dup
}

func (b *fakeBot) purchase(orderID string) (proto.PurchaseRequest, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.purchases[orderID]
	return p, ok
}

// runNode starts a psnode from YAML and returns its admin base URL.
func runNode(t *testing.T, ctx context.Context, yaml string) string {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, yaml)
	}
	n, err := node.New(ctx, cfg, testutil.Logger(t).With("node", cfg.Name))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := n.Run(ctx); err != nil {
			t.Errorf("node %s: %v", cfg.Name, err)
		}
	}()
	t.Cleanup(func() { <-done })
	base := "http://" + cfg.Admin.Listen
	deadline := time.Now().Add(20 * time.Second)
	for {
		if res, err := http.Get(base + "/status"); err == nil {
			res.Body.Close()
			return base
		}
		if time.Now().After(deadline) {
			t.Fatalf("admin API of %s not up", cfg.Name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func admin(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Authorization", "Bearer lab")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode/100 == 2 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, data)
		}
	}
	if res.StatusCode/100 != 2 && out != nil {
		t.Logf("%s %s: %d %s", method, url, res.StatusCode, data)
	}
	return res.StatusCode
}

// user is the scripted browser user: a messenger with the user-1 keys.
type user struct {
	keys *keys.Set
	m    *messenger.Messenger
	mu   sync.Mutex
	got  map[string][]*messenger.Message // order id → messages
	cond *sync.Cond
}

func newUser(t *testing.T, ctx context.Context, relays []string) *user {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u := &user{keys: labKeys(t, "user-1"), got: map[string][]*messenger.Message{}}
	u.cond = sync.NewCond(&u.mu)
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	t.Cleanup(pool.Close)
	u.m, err = messenger.New(messenger.Config{Secret: u.keys.NostrSecretHex(), Pool: pool, DB: db, Inbox: relays, K: 2, Log: testutil.Logger(t).With("node", "user")})
	if err != nil {
		t.Fatal(err)
	}
	u.m.HandleOther(func(_ context.Context, msg *messenger.Message) {
		u.mu.Lock()
		u.got[msg.OrderID] = append(u.got[msg.OrderID], msg)
		u.mu.Unlock()
		u.cond.Broadcast()
	})
	u.m.Start(ctx)
	go func() { <-ctx.Done(); u.cond.Broadcast() }()
	return u
}

// wait returns the first message of a type for an order.
func (u *user) wait(t *testing.T, ctx context.Context, orderID, typ string, v any) *messenger.Message {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for {
		for _, m := range u.got[orderID] {
			if m.Type == typ {
				if v != nil {
					if err := m.Decode(v); err != nil {
						t.Fatal(err)
					}
				}
				return m
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("no %s for order %s (got %v)", typ, orderID, u.types(orderID))
		}
		u.cond.Wait()
	}
}

func (u *user) types(orderID string) []string {
	var out []string
	for _, m := range u.got[orderID] {
		out = append(out, m.Type)
	}
	return out
}

func (u *user) send(t *testing.T, ctx context.Context, to, orderID, typ string, body any) *nostr.Event {
	t.Helper()
	ev, err := u.m.Send(ctx, to, orderID, typ, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func waitUntil(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %s", what)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
