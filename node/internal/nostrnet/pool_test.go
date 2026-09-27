package nostrnet

import (
	"context"
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
}
