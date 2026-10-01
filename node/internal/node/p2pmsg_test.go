package node

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/p2p"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func labKey(name string) string {
	return filepath.Join("..", "..", "..", "lab", "keys", name+".mnemonic")
}

// runNode starts a node of the YAML (data_dir is filled in) and stops it at the end of the test.
func runNode(t *testing.T, yaml string) *Node {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml + "data_dir: " + t.TempDir() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = n.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 10*time.Second, "node start", n.started.Load)
	return n
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func operatorYAML(key, relay, extra string) string {
	return fmt.Sprintf("role: operator\nmnemonic_file: %s\nnostr: {relays: [%q], allow_private_relays: true}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\n%s%s",
		labKey(key), relay, someCoordinator(), extra)
}

func inboxHas(m *messenger.Messenger, id string) bool {
	return slices.ContainsFunc(m.Inbox(""), func(ev *nostr.Event) bool { return ev.ID == id })
}

// Two nodes (in-process libp2p hosts, one Nostr relay): a message goes over /ps/msg/1.0.0 when the recipient's
// address is known, its ack comes back the same way; when P2P fails it goes to the mailbox (§4.2).
func TestTwoNodesP2PMessageAndFallback(t *testing.T) {
	relay := testutil.StartRelay(t)
	a := runNode(t, operatorYAML("operator-1", relay, ""))
	b := runNode(t, operatorYAML("operator-2", relay, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	time.Sleep(300 * time.Millisecond) // the subscriptions open

	bContact := proto.P2PContact{PeerID: b.host.ID().String(), Addrs: b.host.FullAddrs()}
	a.contacts.put(b.keys.NostrPubHex(), bContact)
	// b learns a's address from a's (signed) order request, as a shopper learns the user's reply_p2p
	b.observe(&messenger.Message{Type: proto.TypeOrderRequest, From: a.keys.NostrPubHex(), Inner: &nostr.Event{
		Content: mustJSON(t, proto.OrderRequest{ReplyP2P: &proto.P2PContact{PeerID: a.host.ID().String(), Addrs: a.host.FullAddrs()}}),
	}})

	report := proto.Report{Subject: b.keys.NostrPubHex(), OrderID: "", Text: "over p2p"}
	inner, err := a.msgr.Send(ctx, b.keys.NostrPubHex(), "", proto.TypeReport, report, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, "p2p delivery", func() bool { return inboxHas(b.msgr, inner.ID) })
	if v := a.msgr.DeliveredVia(inner.ID); v != messenger.ViaP2P {
		t.Fatalf("delivered via %q", v)
	}
	waitUntil(t, 10*time.Second, "ack", func() bool { return a.msgr.Acked(inner.ID) })
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pool.Close()
	if evs := pool.Query(ctx, []string{relay}, nostr.Filter{Kinds: []int{giftwrap.KindWrap}}); len(evs) != 0 {
		t.Fatalf("the relay got %d wraps although both went over P2P", len(evs))
	}

	// b's address is stale (another peer id answers nothing): the mailbox
	k, _, _ := crypto.GenerateSecp256k1Key(nil)
	stale := mustPeerID(t, k)
	a.contacts.put(b.keys.NostrPubHex(), proto.P2PContact{PeerID: stale, Addrs: []string{"/ip4/127.0.0.1/tcp/1/p2p/" + stale}})
	report.Text = "via the mailbox"
	inner2, err := a.msgr.Send(ctx, b.keys.NostrPubHex(), "", proto.TypeReport, report, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 15*time.Second, "mailbox delivery", func() bool { return inboxHas(b.msgr, inner2.ID) })
	if v := a.msgr.DeliveredVia(inner2.ID); v != "nostr" {
		t.Fatalf("delivered via %q", v)
	}
	// for a while, messages to b skip the P2P attempt
	if !a.failed.recent(b.keys.NostrPubHex(), time.Now()) {
		t.Fatal("the failed P2P delivery is not remembered")
	}
}

func mustPeerID(t *testing.T, k crypto.PrivKey) string {
	t.Helper()
	id, err := peer.IDFromPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// An invalid reply_p2p is not remembered; a shopper or escrow profile's p2p comes first.
func TestContactOf(t *testing.T) {
	relay := testutil.StartRelay(t)
	n := runNode(t, operatorYAML("operator-1", relay, ""))
	user := newActor()
	n.observe(&messenger.Message{Type: proto.TypeOrderRequest, From: user.pk, Inner: &nostr.Event{
		Content: mustJSON(t, proto.OrderRequest{ReplyP2P: &proto.P2PContact{PeerID: "not-a-peer-id"}}),
	}})
	if c := n.contactOf(user.pk, "0011"); c != nil {
		t.Fatalf("invalid reply_p2p used: %+v", c)
	}
	good := proto.P2PContact{PeerID: n.host.ID().String(), Addrs: []string{"/ip4/127.0.0.1/tcp/4001/p2p/" + n.host.ID().String()}}
	n.observe(&messenger.Message{Type: proto.TypeOrderRequest, From: user.pk, Inner: &nostr.Event{Content: mustJSON(t, proto.OrderRequest{ReplyP2P: &good})}})
	if c := n.contactOf(user.pk, "0011"); c == nil || c.PeerID != good.PeerID {
		t.Fatalf("reply_p2p not used: %+v", c)
	}
	// our own address is never dialled
	if ok, err := n.sendDirect(context.Background(), user.pk, "", &nostr.Event{}); ok || err != nil {
		t.Fatalf("sent to ourselves: %v %v", ok, err)
	}
}

// trust.nostr off (the default, §2.6): the node does not fetch trust events from the relays; on, it does.
func TestTrustNostrSwitch(t *testing.T) {
	old := BridgeDebounce
	BridgeDebounce = 50 * time.Millisecond
	defer func() { BridgeDebounce = old }()
	relay := testutil.StartRelay(t)
	c := newActor()
	n1, _ := operatorKeys(t, "operator-1")
	del, err := trust.NewDelegation(c.sk, n1, "ps-lab", 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Publish(ctx, []string{relay}, del); err != nil {
		t.Fatal(err)
	}
	yaml := func(nostrOn bool) string {
		return fmt.Sprintf("role: operator\nmnemonic_file: %s\nnostr: {relays: [%q], allow_private_relays: true}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\ntrust: {coordinators: [%q], nostr: %v}\n",
			labKey("operator-1"), relay, c.pk, nostrOn)
	}
	off := runNode(t, yaml(false))
	time.Sleep(1500 * time.Millisecond)
	if off.trust.Get(trust.KeyOf(del)) != nil {
		t.Fatal("trust.nostr off, but the delegation came from the relay")
	}
	on := runNode(t, yaml(true))
	waitUntil(t, 10*time.Second, "delegation from the relay", func() bool { return on.trust.Get(trust.KeyOf(del)) != nil })
}

func operatorKeys(t *testing.T, name string) (string, string) {
	t.Helper()
	k, err := keys.LoadMnemonicFile(labKey(name))
	if err != nil {
		t.Fatal(err)
	}
	return k.NostrPubHex(), k.NostrSecretHex()
}

// §2.6: the delegation comes in a trust bundle (bundle_urls), its list_url names the operator's list bundle, which
// the node fetches next; the list's p2p_relays are dialled (§2.3). Only https, with the lab's extra CA.
func TestListURLBundleAndP2PRelays(t *testing.T) {
	c, op, sh, es := newActor(), newActor(), newActor(), newActor()
	relayKey, _, _ := crypto.GenerateSecp256k1Key(nil)
	relayID := mustPeerID(t, relayKey)
	p2pRelay := "/dns4/relay.example/tcp/443/tls/ws/p2p/" + relayID
	var srvURL string
	var listFetches atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trust.json":
			del, err := trust.NewDelegationWithURLs(c.sk, op.pk, "ps-lab", 1, false, "", []string{srvURL + "/list.json"})
			if err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(trust.Bundle{Events: []*nostr.Event{del}})
		case "/list.json":
			listFetches.Add(1)
			entry := trust.Entry{Region: "JP", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}, Tags: []string{}, EscrowSLADays: 14}
			l, err := trust.NewList(op.sk, 3, &trust.List{Network: "ps-lab", Name: "op", Entries: []trust.Entry{entry}, P2PRelays: []string{p2pRelay}})
			if err != nil {
				t.Error(err)
			}
			prof, _ := trust.NewProfile(sh.sk, trust.KindShopperProfile, "ps-lab", 2, map[string]any{"name": "sh", "p2p": map[string]any{"peer_id": relayID, "addrs": []string{}}})
			_ = json.NewEncoder(w).Encode(trust.Bundle{Events: []*nostr.Event{l, prof}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	n := runNode(t, fmt.Sprintf("role: operator\nmnemonic_file: %s\ntls: {extra_ca: %s}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\ntrust: {coordinators: [%q], bundle_urls: [%q]}\n",
		labKey("operator-1"), caFile, c.pk, srv.URL+"/trust.json"))
	waitUntil(t, 10*time.Second, "the list of the list_url", func() bool {
		return len(n.trust.Effective([]string{c.pk}, "ps-lab")) == 1
	})
	waitUntil(t, 5*time.Second, "the shopper profile of the list bundle", func() bool {
		p, _ := n.trust.ShopperProfile(sh.pk, "ps-lab")
		return p != nil
	})
	waitUntil(t, 5*time.Second, "the p2p relay of the list", func() bool {
		return slices.ContainsFunc(n.host.Relays(), func(id peer.ID) bool { return id.String() == relayID })
	})
	if n := listFetches.Load(); n != 1 {
		t.Fatalf("list bundle fetched %d times", n)
	}
}

// The escrow profile carries the circuit addresses of its reservations and is published again (new v) when a
// reservation is added (§3, §10, §12).
func TestProfileRepublishedOnAddressChange(t *testing.T) {
	old := AddrRepublishInterval
	AddrRepublishInterval = 0
	defer func() { AddrRepublishInterval = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relayHost := func() string {
		k, _, _ := crypto.GenerateSecp256k1Key(nil)
		h, err := p2p.NewHost(p2p.Options{Key: k, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Reachability: "public", RelayService: true, Log: testutil.Logger(t)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		h.Start(ctx)
		return h.FullAddrs()[0]
	}
	r1, r2 := relayHost(), relayHost()
	relay := testutil.StartRelay(t)
	n := runNode(t, fmt.Sprintf(`role: escrow
mnemonic_file: %s
nostr: {relays: [%q], allow_private_relays: true}
p2p: {relays: [%q], reachability: private}
%sescrow: {upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}, dispute_fee_bps: 200}
`, labKey("escrow-1"), relay, r1, someCoordinator()))
	profile := func() (*trust.EscrowProfile, *nostr.Event) {
		return n.trust.EscrowProfile(n.keys.NostrPubHex(), n.cfg.Network)
	}
	hasCircuit := func(relayAddr string) bool {
		p, _ := profile()
		return p != nil && p.P2P != nil && slices.ContainsFunc(p.P2P.Addrs, func(a string) bool {
			return strings.HasPrefix(a, relayAddr+"/p2p-circuit/p2p/"+n.host.ID().String())
		})
	}
	waitUntil(t, 20*time.Second, "profile with the circuit of relay 1", func() bool { return hasCircuit(r1) })
	_, first := profile()
	n.host.AddRelays([]string{r2})
	waitUntil(t, 20*time.Second, "profile with the circuit of relay 2", func() bool { return hasCircuit(r2) })
	if _, ev := profile(); trust.Version(ev) <= trust.Version(first) {
		t.Fatalf("republished profile has v %d, the first %d", trust.Version(ev), trust.Version(first))
	}
}
