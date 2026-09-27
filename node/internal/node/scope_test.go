package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func newTestNode(t *testing.T, ctx context.Context, relay, coordinators, extra string) *Node {
	t.Helper()
	yaml := fmt.Sprintf("role: operator\nmnemonic_file: %s\ndata_dir: %s\nnostr: {relays: [%q], allow_private_relays: true}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\ntrust: {coordinators: [%s]}\n%s",
		filepath.Join("..", "..", "..", "lab", "keys", "operator-2.mnemonic"), t.TempDir(), relay, coordinators, extra)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.host.Close(); n.pool.Close(); n.db.Close() })
	return n
}

type actor struct{ sk, pk string }

func newActor() actor {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	return actor{sk, pk}
}

// The Nostr bridge asks only for (and stores only) what the coordinators reach, following the chain of trust as
// it arrives: delegation, then the operator's list, then the listed shopper's profile (§10).
func TestBridgeFollowsTrustScope(t *testing.T) {
	old := BridgeDebounce
	BridgeDebounce = 50 * time.Millisecond
	defer func() { BridgeDebounce = old }()
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, stranger, op, sh, es, user := newActor(), newActor(), newActor(), newActor(), newActor(), newActor()

	pub := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pub.Close()
	must := func(ev *nostr.Event, err error) *nostr.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pub.Publish(ctx, []string{relay}, ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	deleg := must(trust.NewDelegation(c.sk, op.pk, "ps-lab", 1, false, ""))
	list := must(trust.NewList(op.sk, 1, &trust.List{Network: "ps-lab", Entries: []trust.Entry{{Region: "JP-13", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}}}}))
	profile := must(trust.NewProfile(sh.sk, trust.KindShopperProfile, "ps-lab", 1, trust.ShopperProfile{Name: "s"}))
	shRelays := must(trust.NewInboxRelays(sh.sk, []string{relay}, 1))
	foreign := must(trust.NewDelegation(stranger.sk, op.pk, "ps-lab", 1, false, ""))
	userRelays := must(trust.NewInboxRelays(user.sk, []string{relay}, 1))
	strangerProfile := must(trust.NewProfile(user.sk, trust.KindShopperProfile, "ps-lab", 1, trust.ShopperProfile{Name: "u"}))

	n := newTestNode(t, ctx, relay, fmt.Sprintf("%q", c.pk), "")
	n.bridge(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for _, ev := range []*nostr.Event{deleg, list, profile, shRelays} {
		for n.trust.Get(trust.KeyOf(ev)) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("kind %d not bridged", ev.Kind)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	for _, ev := range []*nostr.Event{foreign, userRelays, strangerProfile} {
		if n.trust.Get(trust.KeyOf(ev)) != nil {
			t.Fatalf("kind %d of an author outside the scope stored", ev.Kind)
		}
	}
	// a user's inbox relays are fetched on demand and cached (not stored in the trust store)
	if r := n.inboxes.resolve(user.pk); len(r) != 1 || r[0] != relay || n.trust.Get(trust.KeyOf(userRelays)) != nil {
		t.Fatalf("on-demand 10050: %v", r)
	}
	if r := n.inboxes.resolve(sh.pk); len(r) != 1 {
		t.Fatalf("listed shopper's 10050: %v", r)
	}
}

func TestInboxCacheBounded(t *testing.T) {
	oldSize := InboxCacheSize
	InboxCacheSize = 3
	defer func() { InboxCacheSize = oldSize }()
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := newTestNode(t, ctx, relay, "", "")
	for i := 0; i < 5; i++ {
		n.inboxes.resolve(newActor().pk)
	}
	if n.inboxes.len() != 3 {
		t.Fatalf("cache holds %d, cap 3", n.inboxes.len())
	}
}

func TestAdminTokenRequiredOffLoopback(t *testing.T) {
	base := "role: operator\nmnemonic_file: /k\n"
	for listen, ok := range map[string]bool{
		`{listen: "0.0.0.0:8080"}`: false, `{listen: ":8080"}`: false, `{listen: "172.40.0.30:8080"}`: false,
		`{listen: "127.0.0.1:8080"}`: true, `{listen: "[::1]:8080"}`: true, `{listen: "localhost:8080"}`: true,
		`{listen: "0.0.0.0:8080", token: "t"}`: true, `{listen: ""}`: true,
	} {
		_, err := config.Parse([]byte(base + "admin: " + listen + "\n"))
		if (err == nil) != ok {
			t.Errorf("admin %s: %v", listen, err)
		}
		if err != nil && !strings.Contains(err.Error(), "admin.token") {
			t.Errorf("admin %s: unexpected error %v", listen, err)
		}
	}
}

// Without nostr.allow_private_relays the node does not talk to relays on private addresses (§4.10).
func TestNodeRefusesPrivateRelays(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	yaml := fmt.Sprintf("role: operator\nmnemonic_file: %s\ndata_dir: %s\nnostr: {relays: [%q]}\np2p: {listen: [\"/ip4/127.0.0.1/tcp/0\"]}\n",
		filepath.Join("..", "..", "..", "lab", "keys", "operator-2.mnemonic"), t.TempDir(), relay)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(ctx, cfg, testutil.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer n.host.Close()
	defer n.pool.Close()
	ev, _ := trust.NewInboxRelays(nostr.GeneratePrivateKey(), []string{relay}, 1)
	if ok, err := n.pool.Publish(ctx, []string{relay}, ev); len(ok) != 0 || err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("published to a loopback relay: %v %v", ok, err)
	}
}
