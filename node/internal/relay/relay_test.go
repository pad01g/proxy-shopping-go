package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

func TestRelayPolicyAndPrune(t *testing.T) {
	srv, err := New(Options{DataDir: t.TempDir(), RetentionDays: 1, MaxEventSize: 2000})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	hs := httptest.NewServer(srv)
	defer hs.Close()
	url := "ws" + strings.TrimPrefix(hs.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sk := nostr.GeneratePrivateKey()
	pub := func(kind int, tags nostr.Tags, content string, at time.Time) error {
		ev := nostr.Event{Kind: kind, Tags: tags, Content: content, CreatedAt: nostr.Timestamp(at.Unix())}
		_ = ev.Sign(sk)
		return r.Publish(ctx, ev)
	}
	now := time.Now()
	if err := pub(1, nil, "note", now); err == nil {
		t.Fatal("kind 1 accepted")
	}
	if err := pub(13, nil, "seal", now); err == nil {
		t.Fatal("kind 13 accepted")
	}
	if err := pub(1059, nil, "wrap", now); err == nil {
		t.Fatal("gift wrap without p accepted")
	}
	if err := pub(1059, nostr.Tags{{"p", "00"}}, strings.Repeat("x", 3000), now); err == nil {
		t.Fatal("oversized event accepted")
	}
	if err := pub(1059, nostr.Tags{{"p", "00"}}, "fresh", now); err != nil {
		t.Fatal(err)
	}
	if err := pub(1059, nostr.Tags{{"p", "00"}}, "old", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pub(30501, nostr.Tags{{"d", "ps-lab"}, {"v", "1"}}, "{}", now); err != nil {
		t.Fatal(err)
	}

	n, err := srv.Prune(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	evs, err := r.QuerySync(ctx, nostr.Filter{Kinds: []int{1059}})
	if err != nil || len(evs) != 1 || evs[0].Content != "fresh" {
		t.Fatalf("after prune: %v %v", evs, err)
	}
}
