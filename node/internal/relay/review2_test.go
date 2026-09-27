package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
)

// Replaceable and addressable events are replaced by v, not created_at (§2.1): an older version signed later must
// not evict a newer one (review 2, item 3).
func TestReplaceByVersion(t *testing.T) {
	_, url := startRelay(t, Options{})
	c := dial(t, url)
	a := newSigner()
	now := time.Now()
	for _, kind := range []int{30500, 30501, 30502, 30503, 10050} {
		d := "ps-lab"
		if kind == 30500 {
			d = newSigner().pk
		}
		tags := func(v string) nostr.Tags {
			if kind == 10050 {
				return nostr.Tags{{"v", v}, {"relay", "wss://r" + v}}
			}
			return nostr.Tags{{"d", d}, {"v", v}, {"network", "ps-lab"}}
		}
		high := a.event(kind, tags("5"), "{}", now.Add(-2*time.Minute))
		low := a.event(kind, tags("4"), "{}", now.Add(-time.Minute)) // lower v, newer created_at
		c.publish(high)
		c.publish(low)
		evs, _ := c.query(nostr.Filter{Kinds: []int{kind}, Authors: []string{a.pk}}, false)
		if len(evs) != 1 || evs[0].ID != high.ID {
			t.Fatalf("kind %d: stored %d events (first v %v), want only v 5", kind, len(evs), evs)
		}
		// a higher version replaces it whatever its created_at
		higher := a.event(kind, tags("6"), "{}", now.Add(-3*time.Minute))
		if r := c.publish(higher); r != "" {
			t.Fatalf("kind %d: higher version refused: %s", kind, r)
		}
		evs, _ = c.query(nostr.Filter{Kinds: []int{kind}, Authors: []string{a.pk}}, false)
		if len(evs) != 1 || evs[0].ID != higher.ID {
			t.Fatalf("kind %d: higher version did not replace", kind)
		}
	}
	// other d values of an addressable kind are separate addresses
	x := a.event(30502, nostr.Tags{{"d", "other"}, {"v", "1"}}, "{}", now)
	c.publish(x)
	if evs, _ := c.query(nostr.Filter{Kinds: []int{30502}, Authors: []string{a.pk}}, false); len(evs) != 2 {
		t.Fatalf("%d addressable events, want 2 (two d values)", len(evs))
	}
	// 10050 without v (other clients) still replaces by created_at
	b := newSigner()
	old := b.event(10050, nostr.Tags{{"relay", "wss://a"}}, "", now.Add(-time.Minute))
	nw := b.event(10050, nostr.Tags{{"relay", "wss://b"}}, "", now)
	c.publish(old)
	c.publish(nw)
	if evs, _ := c.query(nostr.Filter{Kinds: []int{10050}, Authors: []string{b.pk}}, false); len(evs) != 1 || evs[0].ID != nw.ID {
		t.Fatal("10050 without v not replaced by created_at")
	}
}

// IPv6 clients are limited per /64 (item 8).
func TestAddressKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":          "203.0.113.5",
		"::ffff:203.0.113.5":   "203.0.113.5",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"not an ip":            "not an ip",
	} {
		if got := addressKey(in); got != want {
			t.Errorf("addressKey(%s) = %s, want %s", in, got, want)
		}
	}
	if DefaultConnsPerMinute < 600 || DefaultEventsPerMinute < 3000 || DefaultReqsPerMinute < 3000 {
		t.Error("per-address defaults too low for clients behind one NAT")
	}
}

// A connection sending more frames than the frame limit gets NOTICEs instead of work, before any parsing or
// signature check (item 8).
func TestFrameLimit(t *testing.T) {
	_, url := startRelay(t, Options{ConnMessagesPerMinute: 5, ConnEventsPerMinute: -1, EventsPerMinute: -1})
	c := dial(t, url)
	for i := 0; i < 8; i++ {
		c.send("CLOSE", "x") // cheap frames: counted all the same
	}
	notices := 0
	deadline := time.Now().Add(3 * time.Second)
	for notices < 3 && time.Now().Before(deadline) {
		if m := c.read(); label(m) == "NOTICE" && strings.Contains(string(m[1]), "rate-limited") {
			notices++
		}
	}
	if notices != 3 {
		t.Fatalf("%d rate limit notices, want 3", notices)
	}
	// another connection is not affected
	a := newSigner()
	if r := dial(t, url).publish(a.event(1059, nostr.Tags{{"p", "00"}}, "x", time.Now())); r != "" {
		t.Fatalf("other connection: %s", r)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// X-Forwarded-For from a peer that is not a trusted proxy is warned about once; /healthz answers (items 8, 10).
func TestProxyWarningAndHealthz(t *testing.T) {
	var logs syncBuffer
	_, url := startRelay(t, Options{Log: slog.New(slog.NewTextHandler(&logs, nil))})
	for i := 0; i < 2; i++ {
		h := http.Header{"X-Forwarded-For": {"198.51.100.7"}}
		ctx := t.Context()
		ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		ws.CloseNow()
	}
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(logs.String(), "not a trusted proxy"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, logs.String())
	}
	res, err := http.Get("http" + strings.TrimPrefix(url, "ws") + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz: %d %s", res.StatusCode, body)
	}
	var info map[string]any
	req, _ := http.NewRequest("GET", "http"+strings.TrimPrefix(url, "ws"), nil)
	req.Header.Set("Accept", "application/nostr+json")
	if res, err := http.DefaultClient.Do(req); err != nil || json.NewDecoder(res.Body).Decode(&info) != nil {
		t.Fatal("NIP-11 broken")
	}
}
