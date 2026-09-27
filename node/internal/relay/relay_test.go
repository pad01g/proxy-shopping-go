package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
)

// client is a minimal NIP-01 client for the tests (go-nostr's Relay races with itself when it is closed).
type client struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
}

func startRelay(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, "ws" + strings.TrimPrefix(hs.URL, "http")
}

func dial(t *testing.T, url string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(4 << 20)
	t.Cleanup(func() { ws.CloseNow() })
	return &client{t: t, ctx: ctx, ws: ws}
}

func (c *client) send(v ...any) {
	c.t.Helper()
	data, _ := json.Marshal(v)
	if err := c.ws.Write(c.ctx, websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) read() []json.RawMessage {
	c.t.Helper()
	_, data, err := c.ws.Read(c.ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	var msg []json.RawMessage
	if err := json.Unmarshal(data, &msg); err != nil || len(msg) == 0 {
		c.t.Fatalf("bad message %s", data)
	}
	return msg
}

func label(m []json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m[0], &s)
	return s
}

// publish sends an event and returns the OK reason, or "" when accepted.
func (c *client) publish(ev nostr.Event) string {
	c.t.Helper()
	c.send("EVENT", ev)
	for {
		m := c.read()
		if label(m) != "OK" || len(m) < 4 {
			continue
		}
		var id, reason string
		var ok bool
		_ = json.Unmarshal(m[1], &id)
		_ = json.Unmarshal(m[2], &ok)
		_ = json.Unmarshal(m[3], &reason)
		if id != ev.ID {
			continue
		}
		if ok {
			return ""
		}
		if reason == "" {
			reason = "refused"
		}
		return reason
	}
}

var subSeq int

// query sends a REQ and collects the events until EOSE; it returns the CLOSED reason if the relay refused it.
func (c *client) query(f nostr.Filter, keepOpen bool) ([]nostr.Event, string) {
	c.t.Helper()
	subSeq++
	id := "s" + strconv.Itoa(subSeq)
	c.send("REQ", id, f)
	var evs []nostr.Event
	for {
		m := c.read()
		var sid string
		if len(m) > 1 {
			_ = json.Unmarshal(m[1], &sid)
		}
		if sid != id {
			continue
		}
		switch label(m) {
		case "EVENT":
			var ev nostr.Event
			_ = json.Unmarshal(m[2], &ev)
			evs = append(evs, ev)
		case "EOSE":
			if !keepOpen {
				c.send("CLOSE", id)
			}
			return evs, ""
		case "CLOSED":
			var reason string
			_ = json.Unmarshal(m[2], &reason)
			return evs, reason
		}
	}
}

type signer struct{ sk, pk string }

func newSigner() signer {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	return signer{sk, pk}
}

func (s signer) event(kind int, tags nostr.Tags, content string, at time.Time) nostr.Event {
	ev := nostr.Event{Kind: kind, Tags: tags, Content: content, CreatedAt: nostr.Timestamp(at.Unix())}
	_ = ev.Sign(s.sk)
	return ev
}

func TestRelayPolicyAndPrune(t *testing.T) {
	srv, url := startRelay(t, Options{RetentionDays: 1, MaxEventSize: 2000})
	c := dial(t, url)
	a := newSigner()
	now := time.Now()
	for _, bad := range []struct {
		name string
		ev   nostr.Event
	}{
		{"kind 0", a.event(0, nil, "{}", now)},
		{"kind 1", a.event(1, nil, "note", now)},
		{"kind 13", a.event(13, nil, "seal", now)},
		{"gift wrap without p", a.event(1059, nil, "wrap", now)},
		{"oversized event", a.event(1059, nostr.Tags{{"p", "00"}}, strings.Repeat("x", 3000), now)},
	} {
		if c.publish(bad.ev) == "" {
			t.Fatalf("%s accepted", bad.name)
		}
	}
	for _, ev := range []nostr.Event{
		a.event(1059, nostr.Tags{{"p", "00"}}, "fresh", now),
		a.event(1059, nostr.Tags{{"p", "00"}}, "old", now.Add(-48*time.Hour)),
		a.event(30501, nostr.Tags{{"d", "ps-lab"}, {"v", "1"}}, "{}", now),
	} {
		if r := c.publish(ev); r != "" {
			t.Fatal(r)
		}
	}

	n, err := srv.Prune(c.ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	evs, _ := c.query(nostr.Filter{Kinds: []int{1059}}, false)
	if len(evs) != 1 || evs[0].Content != "fresh" {
		t.Fatalf("after prune: %v", evs)
	}
}

func TestDeletionOnlyByAuthor(t *testing.T) {
	_, url := startRelay(t, Options{})
	c := dial(t, url)
	owner, other := newSigner(), newSigner()
	now := time.Now()
	target := owner.event(30502, nostr.Tags{{"d", "ps-lab"}, {"v", "1"}}, "{}", now.Add(-time.Minute))
	if r := c.publish(target); r != "" {
		t.Fatal(r)
	}
	addr := fmt.Sprintf("30502:%s:ps-lab", owner.pk)
	byID := other.event(5, nostr.Tags{{"e", target.ID}}, "", now)
	byAddr := other.event(5, nostr.Tags{{"a", addr}}, "", now)
	if c.publish(byID) == "" || c.publish(byAddr) == "" {
		t.Fatal("deletion by another pubkey accepted")
	}
	if evs, _ := c.query(nostr.Filter{IDs: []string{target.ID}}, false); len(evs) != 1 {
		t.Fatal("event deleted by another pubkey")
	}
	own := owner.event(5, nostr.Tags{{"e", target.ID}}, "", now)
	if r := c.publish(own); r != "" {
		t.Fatalf("own deletion refused: %s", r)
	}
	if evs, _ := c.query(nostr.Filter{IDs: []string{target.ID}}, false); len(evs) != 0 {
		t.Fatal("own deletion did not delete")
	}

	// a relay without kind 5 must not delete either (khatru deletes before it applies RejectEvent)
	_, url2 := startRelay(t, Options{Kinds: []int{30502}})
	c2 := dial(t, url2)
	if r := c2.publish(target); r != "" {
		t.Fatal(r)
	}
	c2.publish(own)
	if evs, _ := c2.query(nostr.Filter{IDs: []string{target.ID}}, false); len(evs) != 1 {
		t.Fatal("kind 5 deleted on a relay that does not accept kind 5")
	}
}

func TestReplaceableKeepLatest(t *testing.T) {
	_, url := startRelay(t, Options{})
	c := dial(t, url)
	a := newSigner()
	now := time.Now()
	for _, kind := range []int{10050, 30502} {
		tags := func(v string) nostr.Tags {
			if kind == 10050 {
				return nostr.Tags{{"relay", "wss://" + v}}
			}
			return nostr.Tags{{"d", "ps-lab"}, {"v", v}}
		}
		older := a.event(kind, tags("1"), "{}", now.Add(-2*time.Minute))
		newer := a.event(kind, tags("2"), "{}", now.Add(-time.Minute))
		for _, ev := range []nostr.Event{older, newer, older} {
			c.publish(ev) // the replayed older one may be refused or ignored, never stored
		}
		evs, _ := c.query(nostr.Filter{Kinds: []int{kind}, Authors: []string{a.pk}}, false)
		if len(evs) != 1 || evs[0].ID != newer.ID {
			t.Fatalf("kind %d: stored %d events, want only the latest", kind, len(evs))
		}
	}
}

func TestEventRateLimits(t *testing.T) {
	_, url := startRelay(t, Options{EventsPerMinute: 5, ConnEventsPerMinute: -1})
	a := newSigner()
	pub := func(c *client, i int) string {
		return c.publish(a.event(1059, nostr.Tags{{"p", "00"}}, strconv.Itoa(i), time.Now()))
	}
	c1 := dial(t, url)
	for i := range 5 {
		if r := pub(c1, i); r != "" {
			t.Fatalf("event %d refused: %s", i, r)
		}
	}
	if r := pub(c1, 5); !strings.HasPrefix(r, "rate-limited") {
		t.Fatalf("6th event: %q", r)
	}
	// the limit is per address, a new connection does not reset it
	if r := pub(dial(t, url), 6); !strings.HasPrefix(r, "rate-limited") {
		t.Fatalf("new connection: %q", r)
	}

	_, url2 := startRelay(t, Options{EventsPerMinute: -1, ConnEventsPerMinute: 3})
	c2 := dial(t, url2)
	for i := range 3 {
		if r := pub(c2, i); r != "" {
			t.Fatal(r)
		}
	}
	if r := pub(c2, 3); !strings.HasPrefix(r, "rate-limited") {
		t.Fatalf("4th event on the connection: %q", r)
	}
	if r := pub(dial(t, url2), 4); r != "" {
		t.Fatalf("other connection limited: %s", r)
	}
}

func TestReqLimits(t *testing.T) {
	_, url := startRelay(t, Options{MaxSubscriptions: 2, ReqsPerMinute: -1})
	c := dial(t, url)
	f := nostr.Filter{Kinds: []int{1059}}
	for range 2 {
		if _, r := c.query(f, true); r != "" {
			t.Fatal(r)
		}
	}
	if _, r := c.query(f, true); !strings.Contains(r, "too many open subscriptions") {
		t.Fatalf("3rd open subscription: %q", r)
	}
	// limit:0 filters skip RejectFilter in khatru, they must be counted all the same
	if _, r := c.query(nostr.Filter{Kinds: []int{1059}, LimitZero: true}, true); !strings.Contains(r, "too many open subscriptions") {
		t.Fatalf("limit:0 subscription: %q", r)
	}
	// closed subscriptions free their place
	c2 := dial(t, url)
	for range 5 {
		if _, r := c2.query(f, false); r != "" {
			t.Fatalf("after CLOSE: %s", r)
		}
	}

	_, url2 := startRelay(t, Options{ReqsPerMinute: 3})
	c3 := dial(t, url2)
	for range 3 {
		if _, r := c3.query(f, false); r != "" {
			t.Fatal(r)
		}
	}
	if _, r := c3.query(f, false); !strings.HasPrefix(r, "rate-limited") {
		t.Fatalf("4th REQ: %q", r)
	}
	if _, r := c3.query(nostr.Filter{Search: rejectMarker + "x"}, false); r == "" {
		t.Fatal("search filter accepted")
	}
}

func TestConnectionLimitAndProxies(t *testing.T) {
	_, url := startRelay(t, Options{ConnsPerMinute: 2})
	dial(t, url)
	dial(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ws, res, err := websocket.Dial(ctx, url, nil); err == nil {
		ws.CloseNow()
		t.Fatal("3rd connection of the minute accepted")
	} else if res == nil || res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd connection: %v", err)
	}

	px, err := parseProxies([]string{"10.0.0.1", "192.168.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		remote, xff, want string
	}{
		{"203.0.113.5:1", "198.51.100.1", "203.0.113.5"},                     // an untrusted peer cannot name itself
		{"10.0.0.1:1", "198.51.100.1", "198.51.100.1"},                       // behind the proxy
		{"10.0.0.1:1", "6.6.6.6, 198.51.100.1, 192.168.1.1", "198.51.100.1"}, // the client's own hops are not believed
		{"10.0.0.1:1", "", "10.0.0.1"},
	} {
		r := &http.Request{RemoteAddr: c.remote, Header: http.Header{}}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := px.clientIP(r); got != c.want {
			t.Errorf("clientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestLimiterRefillAndSweep(t *testing.T) {
	l := newLimiter[string](60)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	for range 60 {
		if !l.allow("a") {
			t.Fatal("burst refused")
		}
	}
	if l.allow("a") || l.available("a") {
		t.Fatal("empty bucket allowed")
	}
	now = now.Add(time.Second)
	if !l.allow("a") || l.allow("a") {
		t.Fatal("refill is not one token per second")
	}
	now = now.Add(2 * time.Minute)
	l.allow("b")
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("full bucket not swept")
	}
	if newLimiter[string](-1).allow("x") != true {
		t.Fatal("disabled limiter refused")
	}
}
