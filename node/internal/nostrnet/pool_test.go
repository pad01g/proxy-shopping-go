package nostrnet

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

func TestPublishQuerySubscribe(t *testing.T) {
	relays := []string{testutil.StartRelay(t), testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := NewPool(nil, testutil.Logger(t))
	defer p.Close()

	got := make(chan *nostr.Event, 8)
	p.Subscribe(ctx, relays, nostr.Filters{{Kinds: []int{10050}}}, func(_ string, ev *nostr.Event) { got <- ev })
	time.Sleep(300 * time.Millisecond)

	sk := nostr.GeneratePrivateKey()
	ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"relay", "wss://x"}}}
	_ = ev.Sign(sk)
	ok, err := p.Publish(ctx, append(relays, "ws://127.0.0.1:1"), ev)
	if len(ok) != 2 || err == nil {
		t.Fatalf("published to %v, err %v (the dead relay must be reported)", ok, err)
	}
	select {
	case e := <-got:
		if e.ID != ev.ID {
			t.Fatal("other event")
		}
	case <-ctx.Done():
		t.Fatal("subscription got nothing")
	}
	evs := p.Query(ctx, relays, nostr.Filter{Kinds: []int{10050}})
	if len(evs) != 1 {
		t.Fatalf("query returned %d events, want 1 (deduplicated)", len(evs))
	}
	// a rejected event reports the relay's reason
	bad := &nostr.Event{Kind: 1, CreatedAt: nostr.Now()}
	_ = bad.Sign(sk)
	if ok, err := p.Publish(ctx, relays[:1], bad); len(ok) != 0 || err == nil {
		t.Fatalf("kind 1 accepted: %v %v", ok, err)
	}
}

// A message that reaches the relay while we are disconnected keeps the created_at of its first sending. The
// reconnected subscription must still deliver it (it used to ask only for the last 10 minutes).
func TestReconnectDeliversOldEvents(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := NewPool(nil, testutil.Logger(t))
	defer p.Close()
	got := make(chan *nostr.Event, 8)
	p.Subscribe(ctx, []string{relay}, nostr.Filters{{Kinds: []int{10050}, Limit: 100}}, func(_ string, ev *nostr.Event) { got <- ev })
	waitFor(t, ctx, func() bool { return p.Connections() == 1 })
	time.Sleep(200 * time.Millisecond)

	// drop the connection, and publish from elsewhere while it is down
	p.mu.Lock()
	for _, c := range p.conns {
		c.close(errors.New("test drop"))
	}
	p.mu.Unlock()
	other := NewPool(nil, testutil.Logger(t))
	defer other.Close()
	ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Timestamp(time.Now().Add(-time.Hour).Unix()), Tags: nostr.Tags{{"relay", "wss://x"}}}
	_ = ev.Sign(nostr.GeneratePrivateKey())
	if _, err := other.Publish(ctx, []string{relay}, ev); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.ID != ev.ID {
			t.Fatal("other event")
		}
	case <-ctx.Done():
		t.Fatal("old event not delivered after the reconnect")
	}
}

func TestRefusePrivateRelays(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := NewPoolWith(Options{Log: testutil.Logger(t)})
	defer p.Close()
	ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Now()}
	_ = ev.Sign(nostr.GeneratePrivateKey())
	if ok, err := p.Publish(ctx, []string{relay}, ev); len(ok) != 0 || !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("loopback relay used: %v %v", ok, err)
	}
	if evs := p.Query(ctx, []string{relay, "ws://localhost:1"}, nostr.Filter{Kinds: []int{10050}}); len(evs) != 0 {
		t.Fatal("query reached a loopback relay")
	}
	allowed := NewPoolWith(Options{Log: testutil.Logger(t), AllowPrivate: true})
	defer allowed.Close()
	if ok, err := allowed.Publish(ctx, []string{relay}, ev); len(ok) != 1 {
		t.Fatalf("allowed pool refused: %v", err)
	}

	for addr, private := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.20.0.2": true, "192.168.1.1": true, "169.254.1.1": true,
		"fe80::1": true, "fd00::1": true, "0.0.0.0": true, "100.64.0.1": true, "::ffff:10.0.0.1": true,
		"8.8.8.8": false, "2606:4700::1111": false, "172.32.0.1": false, "172.40.0.2": false,
	} {
		if got := IsPrivateAddr(netip.MustParseAddr(addr)); got != private {
			t.Errorf("IsPrivateAddr(%s) = %v", addr, got)
		}
	}
}

func TestIdleConnectionsEvicted(t *testing.T) {
	relays := []string{testutil.StartRelay(t), testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := NewPool(nil, testutil.Logger(t))
	defer p.Close()
	p.Subscribe(ctx, relays[:1], nostr.Filters{{Kinds: []int{10050}}}, func(string, *nostr.Event) {})
	ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Now()}
	_ = ev.Sign(nostr.GeneratePrivateKey())
	if ok, err := p.Publish(ctx, relays, ev); len(ok) != 2 {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() bool { return p.Connections() == 2 })
	// the subscribed connection stays, the one only published to goes
	if n := p.evictIdle(time.Now().Add(time.Second)); n != 1 || p.Connections() != 1 {
		t.Fatalf("evicted %d, %d left", n, p.Connections())
	}
	// and comes back on demand
	if ok, err := p.Publish(ctx, relays[1:], ev); len(ok) != 1 {
		t.Fatal(err)
	}
}

// Closing the pool while connections are being dialed and subscriptions start must not race (go test -race).
func TestCloseWhileConnecting(t *testing.T) {
	relay := testutil.StartRelay(t)
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		p := NewPool(nil, testutil.Logger(t))
		p.Subscribe(ctx, []string{relay, relay}, nostr.Filters{{Kinds: []int{10050}}}, func(string, *nostr.Event) {})
		var wg sync.WaitGroup
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ev := &nostr.Event{Kind: 10050, CreatedAt: nostr.Now()}
				_ = ev.Sign(nostr.GeneratePrivateKey())
				_, _ = p.Publish(ctx, []string{relay}, ev)
			}()
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		p.Close()
		cancel()
		wg.Wait()
	}
}

func waitFor(t *testing.T, ctx context.Context, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
