package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

type adminClient struct {
	t     *testing.T
	url   string
	token string
}

func (c adminClient) do(method, path string, hdr map[string]string, body string) (int, string) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(data)
}

// The admin API always needs a token (generated into data_dir when none is configured), refuses other Host names
// (DNS rebinding), cross-origin requests and non-JSON POSTs (CSRF); /healthz is open (review 2, items 5 and 10).
func TestAdminGuard(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := newTestNode(t, ctx, relay, "", `admin: {listen: "127.0.0.1:0", hosts: [operator-2]}`+"\n")
	tokenFile := filepath.Join(n.cfg.DataDir, AdminTokenFile)
	data, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(data)) != n.adminToken || len(n.adminToken) != 64 {
		t.Fatalf("generated token %q, file %q (%v)", n.adminToken, data, err)
	}
	if fi, _ := os.Stat(tokenFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	// the same token after a restart
	if again, _ := adminToken(n.cfg, n.log); again != n.adminToken {
		t.Fatal("token changed on restart")
	}
	srv := httptest.NewServer(n.adminHandler())
	defer srv.Close()
	c := adminClient{t, srv.URL, n.adminToken}
	anon := adminClient{t, srv.URL, ""}

	if code, _ := anon.do("GET", "/status", nil, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := c.do("GET", "/status", nil, ""); code != 200 {
		t.Fatalf("with token: %d", code)
	}
	for host, want := range map[string]int{"localhost:8080": 200, "operator-2:8080": 200, "172.40.0.51:8080": 200, "[::1]:8080": 200, "evil.example:8080": 403, "operator-2.evil.example": 403} {
		if code, _ := c.do("GET", "/status", map[string]string{"Host": host}, ""); code != want {
			t.Errorf("Host %s: %d, want %d", host, code, want)
		}
	}
	u := strings.TrimPrefix(srv.URL, "http://")
	if code, _ := c.do("GET", "/status", map[string]string{"Origin": "http://" + u}, ""); code != 200 {
		t.Fatalf("same origin: %d", code)
	}
	if code, _ := c.do("POST", "/p2p/status", map[string]string{"Origin": "https://evil.example"}, `{}`); code != http.StatusForbidden {
		t.Fatalf("cross origin POST: %d", code)
	}
	if code, _ := c.do("POST", "/events", map[string]string{"Content-Type": "text/plain"}, `[]`); code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST: %d", code)
	}
	if code, body := anon.do("GET", "/healthz", map[string]string{"Host": "whatever"}, ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "false") {
		t.Fatalf("healthz before start: %d %s", code, body)
	}
	n.started.Store(true)
	if code, body := anon.do("GET", "/healthz", nil, ""); code != 200 || !strings.Contains(body, `"ok": true`) {
		t.Fatalf("healthz: %d %s", code, body)
	}
}

// POST /admin/pause stops the message traffic for the given time; /admin/resume ends it (item 11).
func TestAdminPause(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := newTestNode(t, ctx, relay, "", "")
	n.adminToken = "t"
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = n.Run(rctx) }()
	defer func() { stop(); <-done }()
	for !n.started.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	srv := httptest.NewServer(n.adminHandler())
	defer srv.Close()
	c := adminClient{t, srv.URL, "t"}
	if code, _ := c.do("POST", "/admin/pause", nil, `{"seconds": 0}`); code != http.StatusBadRequest {
		t.Fatalf("pause 0 s: %d", code)
	}
	code, body := c.do("POST", "/admin/pause", nil, `{"seconds": 60}`)
	var res struct {
		Paused bool  `json:"paused"`
		Until  int64 `json:"paused_until"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil || !res.Paused || res.Until < time.Now().Unix()+50 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if !n.msgr.Paused() {
		t.Fatal("messenger not paused")
	}
	var st Status
	_, body = c.do("GET", "/status", nil, "")
	if json.Unmarshal([]byte(body), &st) != nil || st.PausedUntil != res.Until {
		t.Fatalf("status while paused: %s", body)
	}
	if code, body := c.do("POST", "/admin/resume", nil, `{}`); code != 200 || !strings.Contains(body, `"was_paused": true`) {
		t.Fatalf("resume: %d %s", code, body)
	}
	if n.msgr.Paused() || !n.PausedUntil().IsZero() {
		t.Fatal("still paused after resume")
	}
	// a short pause ends by itself
	if _, err := n.Pause(time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n.msgr.Paused() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n.msgr.Paused() {
		t.Fatal("pause did not end")
	}
}

// Roles accept messages from non-counterparties only of the kinds they take (§4.10, item 4).
func TestAcceptsPolicy(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := newTestNode(t, ctx, relay, "", "") // an operator
	user := nostr.GeneratePrivateKey()
	msg := func(typ string, body any) *messenger.Message {
		inner, err := giftwrap.NewInner(user, n.keys.NostrPubHex(), "0001", typ, body, nostr.Now())
		if err != nil {
			t.Fatal(err)
		}
		return &messenger.Message{Inner: inner, From: inner.PubKey, Type: typ, OrderID: "0001"}
	}
	if !n.accepts(msg(proto.TypeReport, proto.Report{Subject: "x"})) || n.accepts(msg(proto.TypeChat, proto.Chat{Text: "x"})) {
		t.Fatal("operator policy")
	}
	if n.hasOrderWith("anyone", "0001") {
		t.Fatal("an operator has no orders")
	}
	if !requestNames(msg(proto.TypeOrderRequest, proto.OrderRequest{Escrow: "e"}).Inner, "0001", "e") ||
		requestNames(msg(proto.TypeOrderRequest, proto.OrderRequest{Escrow: "other"}).Inner, "0001", "e") ||
		requestNames(msg(proto.TypeOrderRequest, proto.OrderRequest{Escrow: "e"}).Inner, "0002", "e") {
		t.Fatal("requestNames")
	}
	if h := n.hints(msg(proto.TypeOrderRequest, proto.OrderRequest{Relays: []string{"wss://user"}})); len(h) != 1 || h[0] != "wss://user" {
		t.Fatalf("hints of a request: %v", h)
	}
}

// A p2p relay needs no coordinators and keeps every valid event (item 6).
func TestRelayRoleHasNoScope(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	yaml := "role: relay\nmnemonic_file: " + filepath.Join("..", "..", "..", "lab", "keys", "relay-p2p.mnemonic") +
		"\ndata_dir: " + t.TempDir() + "\nnostr: {relays: [\"" + relay + "\"], allow_private_relays: true}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.host.Close(); n.pool.Close(); n.db.Close() })
	ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"relay", "wss://x"}}}
	_ = ev.Sign(nostr.GeneratePrivateKey())
	if _, err := n.trust.Put(ev); err != nil {
		t.Fatalf("relay refused a valid event: %v", err)
	}
}
