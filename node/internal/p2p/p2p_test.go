package p2p

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

type node struct {
	h   *Host
	svc *Service
	st  *trust.Store
}

func startNode(t *testing.T, ctx context.Context, o Options, role string) *node {
	t.Helper()
	k, _, err := crypto.GenerateSecp256k1Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	o.Key, o.Log = k, testutil.Logger(t)
	h, err := NewHost(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	st, _ := trust.NewStore(nil)
	svc, err := NewService(ctx, ServiceOptions{Host: h, Store: st, Network: "ps-test", Secret: nostr.GeneratePrivateKey(), Role: role, Log: o.Log})
	if err != nil {
		t.Fatal(err)
	}
	h.Start(ctx)
	return &node{h, svc, st}
}

func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %s", what)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestGossipSyncStatusThroughRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	relay := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0/ws"}, Reachability: "public", RelayService: true}, "relay")
	relayAddr := relay.h.FullAddrs()[0]

	// an event known before the others connect arrives by trust-sync
	coord := nostr.GeneratePrivateKey()
	op := nostr.GeneratePrivateKey()
	opPub, _ := nostr.GetPublicKey(op)
	d1, _ := trust.NewDelegation(coord, opPub, "ps-test", 1, false, "")
	if _, err := relay.st.Put(d1); err != nil {
		t.Fatal(err)
	}

	a := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Bootstrap: []string{relayAddr}, Relays: []string{relayAddr}}, "shopper")
	// b listens nowhere: it can only be reached through a circuit
	b := startNode(t, ctx, Options{Bootstrap: []string{relayAddr}, Relays: []string{relayAddr}, Reachability: "private"}, "escrow")

	waitFor(t, ctx, "trust-sync", func() bool { return a.st.Get(trust.KeyOf(d1)) != nil && b.st.Get(trust.KeyOf(d1)) != nil })

	// gossip: a publishes a newer version, b and the relay store it and report it as new
	newFromB := make(chan *nostr.Event, 4)
	b.svc.OnNewEvent(func(ev *nostr.Event, _ string) { newFromB <- ev })
	d2, _ := trust.NewDelegation(coord, opPub, "ps-test", 2, true, "")
	if _, err := a.st.Put(d2); err != nil {
		t.Fatal(err)
	}
	// the mesh needs a moment after connecting
	deadline := time.Now().Add(30 * time.Second)
	for b.st.Get(trust.KeyOf(d2)).ID != d2.ID && time.Now().Before(deadline) {
		_ = a.svc.Publish(ctx, d2)
		time.Sleep(time.Second)
	}
	if b.st.Get(trust.KeyOf(d2)).ID != d2.ID {
		t.Fatal("gossip did not reach b")
	}
	select {
	case ev := <-newFromB:
		if ev.ID != d2.ID {
			t.Fatal("wrong event reported")
		}
	case <-ctx.Done():
		t.Fatal("new event not reported")
	}

	// status through the circuit (b has no direct address)
	waitFor(t, ctx, "relay reservation", func() bool { return len(b.h.CircuitAddrs()) > 0 })
	ev, st, err := a.svc.QueryStatus(ctx, b.h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if st.Role != "escrow" || st.Network != "ps-test" || st.Reachability != "private" || ev.Kind != trust.KindStatus {
		t.Fatalf("status %+v", st)
	}
}

// A node with coordinators stores (and so gossips and bridges) only what they reach (§10).
func TestSyncKeepsOnlyScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relay := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Reachability: "public"}, "relay")
	coord, stranger, op := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	coordPub, _ := nostr.GetPublicKey(coord)
	opPub, _ := nostr.GetPublicKey(op)
	good, _ := trust.NewDelegation(coord, opPub, "ps-test", 1, false, "")
	bad, _ := trust.NewDelegation(stranger, opPub, "ps-test", 1, false, "")
	userRelays, _ := trust.NewInboxRelays(nostr.GeneratePrivateKey(), []string{"wss://x"}, 1)
	for _, ev := range []*nostr.Event{good, bad, userRelays} {
		if _, err := relay.st.Put(ev); err != nil {
			t.Fatal(err)
		}
	}
	a := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Bootstrap: []string{relay.h.FullAddrs()[0]}}, "shopper")
	a.st.SetScope([]string{coordPub}, "ps-test")
	waitFor(t, ctx, "trust-sync", func() bool { return a.st.Get(trust.KeyOf(good)) != nil })
	if a.st.Get(trust.KeyOf(bad)) != nil || a.st.Get(trust.KeyOf(userRelays)) != nil {
		t.Fatal("events outside the scope stored")
	}
	var msg pubsub.Message
	msg.Message = &pb.Message{Data: mustJSON(t, bad)}
	if a.svc.validator(TopicTrust("ps-test"))(ctx, relay.h.ID(), &msg) == pubsub.ValidationAccept {
		t.Fatal("gossip validator passes an event outside the scope")
	}
	msg.Message = &pb.Message{Data: mustJSON(t, good)}
	if a.svc.validator(TopicTrust("ps-test"))(ctx, relay.h.ID(), &msg) != pubsub.ValidationAccept {
		t.Fatal("gossip validator refuses an event in the scope")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Events that arrive (by trust-sync) before the delegation that brings their authors into the scope are not lost:
// they are parked, and a change of the scope syncs again with the peers (review 2, item 7).
func TestOutOfOrderEventsKept(t *testing.T) {
	old, oldDelay := trust.MaxParked, ScopeResyncDelay
	trust.MaxParked, ScopeResyncDelay = 1, 200*time.Millisecond // parking alone cannot hold both
	t.Cleanup(func() { trust.MaxParked, ScopeResyncDelay = old, oldDelay })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relay := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Reachability: "public"}, "relay")
	coord, op, sh, es := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	coordPub, _ := nostr.GetPublicKey(coord)
	opPub, _ := nostr.GetPublicKey(op)
	shPub, _ := nostr.GetPublicKey(sh)
	esPub, _ := nostr.GetPublicKey(es)
	list, _ := trust.NewList(op, 1, &trust.List{Network: "ps-test", Entries: []trust.Entry{{Region: "JP", Shopper: shPub, Escrow: esPub, Shops: []string{"*"}, Payments: []string{"btc-signet"}}}})
	profile, _ := trust.NewProfile(sh, trust.KindShopperProfile, "ps-test", 1, trust.ShopperProfile{Name: "s"})
	for _, ev := range []*nostr.Event{list, profile} {
		if _, err := relay.st.Put(ev); err != nil {
			t.Fatal(err)
		}
	}
	a := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}}, "shopper")
	a.st.SetScope([]string{coordPub}, "ps-test")
	if err := a.h.Connect(ctx, peerInfo(t, relay.h.FullAddrs()[0])); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "first trust-sync", func() bool { return a.st.Parked() == 1 })
	// the delegation arrives some other way (the Nostr bridge, say)
	d, _ := trust.NewDelegation(coord, opPub, "ps-test", 1, false, "")
	if _, err := a.st.Put(d); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "list and profile after the scope grew", func() bool {
		return a.st.Get(trust.KeyOf(list)) != nil && a.st.Get(trust.KeyOf(profile)) != nil
	})
}

// A node without a scope (the p2p relay) passes on every valid event, within a per-peer quota (item 6).
func TestRelayQuota(t *testing.T) {
	old := PeerEventsPerMinute
	PeerEventsPerMinute = 3
	t.Cleanup(func() { PeerEventsPerMinute = old })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	relay := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Reachability: "public"}, "relay")
	v := relay.svc.validator(TopicProfiles("ps-test"))
	var results []pubsub.ValidationResult
	for i := 0; i < 5; i++ {
		ev, _ := trust.NewInboxRelays(nostr.GeneratePrivateKey(), []string{"wss://x"}, 1)
		var msg pubsub.Message
		msg.Message = &pb.Message{Data: mustJSON(t, ev)}
		results = append(results, v(ctx, "peer-a", &msg))
	}
	for i, want := range []pubsub.ValidationResult{pubsub.ValidationAccept, pubsub.ValidationAccept, pubsub.ValidationAccept, pubsub.ValidationIgnore, pubsub.ValidationIgnore} {
		if results[i] != want {
			t.Fatalf("event %d: %v, want %v", i, results[i], want)
		}
	}
	ev, _ := trust.NewInboxRelays(nostr.GeneratePrivateKey(), []string{"wss://x"}, 1)
	var msg pubsub.Message
	msg.Message = &pb.Message{Data: mustJSON(t, ev)}
	if v(ctx, "peer-b", &msg) != pubsub.ValidationAccept {
		t.Fatal("another peer's quota used up")
	}
}

func peerInfo(t *testing.T, addr string) peer.AddrInfo {
	t.Helper()
	info, err := peer.AddrInfoFromString(addr)
	if err != nil {
		t.Fatal(err)
	}
	return *info
}
