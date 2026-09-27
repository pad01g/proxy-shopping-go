package nostrnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
)

// fakeRelay answers the first REQ with the events built by events(subscription id), then keeps the connection.
func fakeRelay(t *testing.T, events func(sid string) []any) string {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var req []json.RawMessage
			if json.Unmarshal(data, &req) != nil || len(req) < 2 {
				continue
			}
			var label, sid string
			_ = json.Unmarshal(req[0], &label)
			_ = json.Unmarshal(req[1], &sid)
			if label != "REQ" {
				continue
			}
			for _, m := range events(sid) {
				msg, _ := json.Marshal(m)
				if ws.Write(r.Context(), websocket.MessageText, msg) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(hs.Close)
	return "ws" + strings.TrimPrefix(hs.URL, "http")
}

// A relay sending a validly signed event under a forged id (the id of another event) must not get it delivered:
// the receiver would remember the forged id as seen and drop the real event later (review 2, item 1).
func TestForgedIDNotDelivered(t *testing.T) {
	victim := nostr.GeneratePrivateKey()
	vpk, _ := nostr.GetPublicKey(victim)
	realID := strings.Repeat("ab", 32)
	good := nostr.Event{Kind: 1059, Tags: nostr.Tags{{"p", vpk}}, Content: "real", CreatedAt: nostr.Now()}
	_ = good.Sign(nostr.GeneratePrivateKey())
	url := fakeRelay(t, func(sid string) []any {
		ev := nostr.Event{Kind: 1059, Tags: nostr.Tags{{"p", vpk}}, Content: "junk", CreatedAt: nostr.Now()}
		_ = ev.Sign(nostr.GeneratePrivateKey())
		ev.ID = realID // forged id, valid signature over the content
		return []any{[]any{"EVENT", sid, ev}, []any{"EVENT", sid, good}}
	})
	p := NewPool(nil, nil)
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan *nostr.Event, 4)
	p.Subscribe(ctx, []string{url}, nostr.Filters{{Kinds: []int{1059}, Tags: nostr.TagMap{"p": {vpk}}}}, func(_ string, ev *nostr.Event) { got <- ev })
	select {
	case ev := <-got:
		if ev.ID != good.ID || !ev.CheckID() {
			t.Fatalf("delivered event with id %s (CheckID=%v)", ev.ID, ev.CheckID())
		}
	case <-ctx.Done():
		t.Fatal("the honest event was not delivered")
	}
	select {
	case ev := <-got:
		t.Fatalf("second delivery %s", ev.ID)
	case <-time.After(300 * time.Millisecond):
	}
}

// A query holds at most the filter's limit and no duplicates, whatever the relay sends (review 2, item 9).
func TestQueryLimitAndDedupe(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	var evs []nostr.Event
	for i := 0; i < 5; i++ {
		ev := nostr.Event{Kind: 10050, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"relay", "wss://x"}}, Content: strings.Repeat("x", i)}
		_ = ev.Sign(sk)
		evs = append(evs, ev)
	}
	url := fakeRelay(t, func(sid string) []any {
		var out []any
		for _, ev := range evs {
			out = append(out, []any{"EVENT", sid, ev}, []any{"EVENT", sid, ev}) // each twice
		}
		return append(out, []any{"EOSE", sid})
	})
	p := NewPool(nil, nil)
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := p.Query(ctx, []string{url}, nostr.Filter{Kinds: []int{10050}}); len(got) != 5 {
		t.Fatalf("got %d events, want 5 without duplicates", len(got))
	}
	if got := p.Query(ctx, []string{url}, nostr.Filter{Kinds: []int{10050}, Limit: 3}); len(got) != 3 {
		t.Fatalf("got %d events over the limit 3", len(got))
	}
}

// A relay answering every REQ with CLOSED is asked again with a growing delay, not every second (item 9).
func TestClosedBacksOff(t *testing.T) {
	var mu sync.Mutex
	reqs := 0
	url := fakeRelay(t, func(sid string) []any {
		mu.Lock()
		reqs++
		mu.Unlock()
		return []any{[]any{"CLOSED", sid, "rate-limited: slow down"}}
	})
	p := NewPool(nil, nil)
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
	defer cancel()
	p.Subscribe(ctx, []string{url}, nostr.Filters{{Kinds: []int{1059}}}, func(string, *nostr.Event) {})
	<-ctx.Done()
	mu.Lock()
	defer mu.Unlock()
	// with backoff: t = 0, 1, 3 (then 7); without, t = 0, 1, 2, 3, 4
	if reqs > 3 {
		t.Fatalf("%d REQs in 4.5 s after CLOSED: no backoff", reqs)
	}
}

// The pool keeps at most MaxConns connections, closing idle ones to make room (item 9).
func TestMaxConns(t *testing.T) {
	var urls []string
	for i := 0; i < 3; i++ {
		urls = append(urls, fakeRelay(t, func(sid string) []any { return []any{[]any{"EOSE", sid}} }))
	}
	p := NewPoolWith(Options{AllowPrivate: true, MaxConns: 2})
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// two long subscriptions hold both places
	sctx, stop := context.WithCancel(ctx)
	p.Subscribe(sctx, urls[:2], nostr.Filters{{Kinds: []int{1059}}}, func(string, *nostr.Event) {})
	for p.Connections() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := p.relay(ctx, urls[2]); !errors.Is(err, ErrTooManyConns) {
		t.Fatalf("third relay: %v, want ErrTooManyConns", err)
	}
	// once they are idle, one is closed for the new relay
	stop()
	time.Sleep(200 * time.Millisecond)
	if _, err := p.relay(ctx, urls[2]); err != nil {
		t.Fatalf("third relay after the others went idle: %v", err)
	}
	if n := p.Connections(); n > 2 {
		t.Fatalf("%d connections, cap 2", n)
	}
}

func TestAllowed(t *testing.T) {
	strict := NewPoolWith(Options{})
	defer strict.Close()
	open := NewPool(nil, nil)
	defer open.Close()
	for url, want := range map[string]bool{
		"wss://relay.example": true, "ws://relay.example:7777": true, "https://relay.example": false, "wss://": false,
		"wss://127.0.0.1": false, "ws://localhost:7777": false, "wss://10.1.2.3": false, "wss://[fd00::1]": false,
		"wss://192.168.0.1:443": false, "wss://8.8.8.8": true,
	} {
		if got := strict.Allowed(url); got != want {
			t.Errorf("Allowed(%q) = %v", url, got)
		}
	}
	if !open.Allowed("ws://127.0.0.1:7777") || open.Allowed("http://x") {
		t.Error("a pool allowing private addresses")
	}
}
