package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func TestAdminAPI(t *testing.T) {
	relay := testutil.StartRelay(t)
	coord := nostr.GeneratePrivateKey()
	coordPub, _ := nostr.GetPublicKey(coord)
	yaml := fmt.Sprintf(`role: operator
name: operator-1
network: ps-lab
mnemonic_file: %s
data_dir: %s
admin: {listen: "", token: "secret"}
nostr: {relays: [%q], allow_private_relays: true}
p2p: {listen: ["/ip4/127.0.0.1/tcp/0"]}
trust: {coordinators: [%q]}
`, filepath.Join("..", "..", "..", "lab", "keys", "operator-1.mnemonic"), t.TempDir(), relay, coordPub)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer n.host.Close()
	srv := httptest.NewServer(n.adminHandler())
	defer srv.Close()

	do := func(method, path, token, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(data)
	}
	if code, _ := do("GET", "/status", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	code, body := do("GET", "/status", "secret", "")
	var st Status
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Role != "operator" || st.PeerID != "16Uiu2HAmPAUSpgpbqd2nxh1o21uDytdVMi77PjLD2zBRG5Y7UCCp" {
		t.Fatalf("status %d %s", code, body)
	}

	ev, _ := trust.NewDelegation(coord, n.keys.NostrPubHex(), "ps-lab", 1, false, "")
	data, _ := json.Marshal(ev)
	if code, body := do("POST", "/events", "secret", string(data)); code != 200 || !strings.Contains(body, `"newer": true`) {
		t.Fatalf("post event %d %s", code, body)
	}
	bad := *ev
	bad.Content = "x"
	data, _ = json.Marshal([]*nostr.Event{&bad})
	if code, _ := do("POST", "/events", "secret", string(data)); code != http.StatusBadRequest {
		t.Fatalf("invalid event answered %d", code)
	}
	// null entries and events of authors outside the trust scope are refused
	if code, body := do("POST", "/events", "secret", "[null]"); code != http.StatusBadRequest || !strings.Contains(body, "null event") {
		t.Fatalf("[null] answered %d %s", code, body)
	}
	stranger, _ := trust.NewDelegation(nostr.GeneratePrivateKey(), n.keys.NostrPubHex(), "ps-lab", 1, false, "")
	data, _ = json.Marshal(stranger)
	if code, body := do("POST", "/events", "secret", string(data)); code != http.StatusBadRequest || !strings.Contains(body, "not reachable") {
		t.Fatalf("delegation of another coordinator answered %d %s", code, body)
	}
	if code, body := do("GET", "/trust", "secret", ""); code != 200 || !strings.Contains(body, ev.ID) {
		t.Fatalf("trust %d", code)
	}
	if code, body := do("GET", "/reports", "secret", ""); code != 200 || strings.TrimSpace(body) != "[]" && strings.TrimSpace(body) != "null" {
		t.Fatalf("reports %d %s", code, body)
	}
	if code, _ := do("GET", "/orders", "secret", ""); code != http.StatusNotFound {
		t.Fatalf("operator serves /orders: %d", code)
	}
	// the delegation reached the relay
	evs := n.pool.Query(ctx, []string{relay}, nostr.Filter{Kinds: []int{trust.KindDelegation}})
	if len(evs) != 1 || evs[0].ID != ev.ID {
		t.Fatalf("relay has %d delegations", len(evs))
	}
}

func TestPublishVersioned(t *testing.T) {
	relay := testutil.StartRelay(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf("role: operator\nmnemonic_file: %s\ndata_dir: %s\nnostr: {relays: [%q], allow_private_relays: true}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\n%s",
		filepath.Join("..", "..", "..", "lab", "keys", "operator-2.mnemonic"), t.TempDir(), relay, someCoordinator())))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer n.host.Close()
	if err := n.publishOwn(ctx); err != nil {
		t.Fatal(err)
	}
	first := n.trust.Get(trust.Key{Kind: trust.KindInboxRelays, PubKey: n.keys.NostrPubHex()})
	if err := n.publishOwn(ctx); err != nil {
		t.Fatal(err)
	}
	again := n.trust.Get(trust.Key{Kind: trust.KindInboxRelays, PubKey: n.keys.NostrPubHex()})
	if first == nil || again.ID != first.ID {
		t.Fatal("unchanged inbox relays were signed again")
	}
	n.cfg.Nostr.Relays = append(n.cfg.Nostr.Relays, "wss://other.test")
	if err := n.publishOwn(ctx); err != nil {
		t.Fatal(err)
	}
	changed := n.trust.Get(trust.Key{Kind: trust.KindInboxRelays, PubKey: n.keys.NostrPubHex()})
	if changed.ID == first.ID || trust.Version(changed) <= trust.Version(first) || len(trust.InboxRelays(changed)) != 2 {
		t.Fatal("changed relays not published as a newer version")
	}
}

// someCoordinator is a trust section with a random coordinator (every role but relay needs one).
func someCoordinator() string {
	pk, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	return fmt.Sprintf("trust: {coordinators: [%q]}\n", pk)
}
