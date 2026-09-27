package messenger

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// A relay forwarding junk under the id of a real wrap must not make us drop the real one (review 2, item 1; the
// reviewer's PoC).
func TestWrapIDPoisoning(t *testing.T) {
	db := openDB(t)
	aliceSK := nostr.GeneratePrivateKey()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bob := offline(t, db, nostr.GeneratePrivateKey(), nil)
	got := make(chan *Message, 4)
	bob.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	bob.Start(ctx)
	real := chat(t, aliceSK, bob.Pub, "0001", "real")
	// a malicious relay forwards a junk event under the real wrap's id (signature valid over the junk)
	fake := nostr.Event{Kind: 1059, Tags: nostr.Tags{{"p", bob.Pub}}, Content: "junk", CreatedAt: nostr.Now()}
	_ = fake.Sign(nostr.GeneratePrivateKey())
	fake.ID = real.ID
	bob.receive(ctx, "wss://evil", &fake)
	if bob.db.Has(bucketWraps, real.ID) {
		t.Fatal("forged wrap id remembered as seen")
	}
	bob.receive(ctx, "wss://honest", real)
	if msg := recv(t, got); msg.OrderID != "0001" {
		t.Fatalf("got %+v", msg)
	}
}

// A message whose wrap would exceed what relays store is refused when sending (item 2, §4.9).
func TestWrapSizeLimit(t *testing.T) {
	m := offline(t, openDB(t), nostr.GeneratePrivateKey(), nil)
	bobPub, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	ctx := context.Background()
	// an inner of about 29000 bytes: over the NIP-44 padding step at 28672, its wrap content is over 65535
	if _, err := m.Send(ctx, bobPub, "0001", proto.TypeChat, proto.Chat{Text: strings.Repeat("a", 28650)}, nil); err == nil {
		t.Fatal("a message whose wrap relays refuse was sent")
	}
	inner, err := m.Send(ctx, bobPub, "0001", proto.TypeChat, proto.Chat{Text: strings.Repeat("a", 27000)}, nil)
	if err != nil {
		t.Fatalf("a message within the limit: %v", err)
	}
	var e outboxEntry
	if ok, _ := m.db.Get(bucketOutbox, inner.ID, &e); !ok || len(e.Wrap.Content) > giftwrap.MaxWrapContent {
		t.Fatalf("wrap of %d bytes", len(e.Wrap.Content))
	}
}

// Copies of a stored message (resends in new wraps) cost no rate limit tokens (item 4).
func TestDuplicatesCostNoTokens(t *testing.T) {
	oldRate := SenderRate
	SenderRate = 1
	defer func() { SenderRate = oldRate }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bob := offline(t, openDB(t), nostr.GeneratePrivateKey(), nil)
	var n atomic.Int32
	bob.HandleOther(func(context.Context, *Message) { n.Add(1) })
	bob.Start(ctx)
	alice := nostr.GeneratePrivateKey()
	inner, _ := giftwrap.NewInner(alice, bob.Pub, "0001", proto.TypeChat, proto.Chat{Text: "once"}, nostr.Now())
	for i := 0; i < SenderBurst+10; i++ { // the sender resends the same inner in new wraps
		w, _ := giftwrap.Wrap(alice, inner, giftwrap.Options{})
		bob.receive(ctx, "wss://r", w)
	}
	bob.receive(ctx, "wss://r", chat(t, alice, bob.Pub, "0001", "next"))
	waitFor(t, ctx, func() bool { return n.Load() == 2 })
}

// A counterparty (a party of an order we keep) is not rate limited; strangers share a global cap (item 4).
func TestCounterpartyExemptAndStrangerCap(t *testing.T) {
	oldRate, oldStranger := SenderRate, StrangerRate
	SenderRate, StrangerRate = 1, 1 // per minute: no refill worth speaking of while the test runs
	defer func() { SenderRate, StrangerRate = oldRate, oldStranger }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	party := nostr.GeneratePrivateKey()
	partyPub, _ := nostr.GetPublicKey(party)
	bob := offline(t, openDB(t), nostr.GeneratePrivateKey(), func(pk, orderID string) bool { return pk == partyPub && orderID == "0001" })
	var fromParty, fromStrangers atomic.Int32
	bob.HandleOther(func(_ context.Context, msg *Message) {
		if msg.From == partyPub {
			fromParty.Add(1)
		} else {
			fromStrangers.Add(1)
		}
	})
	bob.Start(ctx)
	for i := 0; i < SenderBurst+20; i++ {
		bob.receive(ctx, "wss://r", chat(t, party, bob.Pub, "0001", fmt.Sprint("party ", i)))
	}
	for i := 0; i < StrangerBurst+15; i++ { // one message each from many strangers
		bob.receive(ctx, "wss://r", chat(t, nostr.GeneratePrivateKey(), bob.Pub, "", "hi"))
	}
	waitFor(t, ctx, func() bool {
		return fromParty.Load() == int32(SenderBurst+20) && fromStrangers.Load() == int32(StrangerBurst)
	})
	time.Sleep(200 * time.Millisecond)
	if fromStrangers.Load() != int32(StrangerBurst) {
		t.Fatalf("%d messages from strangers, global cap %d", fromStrangers.Load(), StrangerBurst)
	}
}

// What no role accepts from a stranger is neither stored nor acked; the same message from a counterparty is (item 4).
func TestNotAcceptedIsNotStored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := openDB(t)
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	t.Cleanup(pool.Close)
	var isParty atomic.Bool
	bob, err := New(Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: db, Log: testutil.Logger(t),
		Retry:   func(string, string) bool { return isParty.Load() },
		Accepts: func(msg *Message) bool { return msg.Type == proto.TypeOrderRequest }})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Message, 4)
	bob.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	w := chat(t, nostr.GeneratePrivateKey(), bob.Pub, "0001", "hello stranger")
	bob.receive(ctx, "wss://r", w)
	if len(bob.Inbox("")) != 0 || bob.PendingAcks() != 0 || bob.db.Has(bucketWraps, w.ID) {
		t.Fatal("a message no role accepts was stored, acked or remembered")
	}
	isParty.Store(true)
	bob.receive(ctx, "wss://r", w) // e.g. at the next reconnect, once the order exists
	if len(bob.Inbox("")) != 1 || bob.PendingAcks() != 1 {
		t.Fatal("the counterparty's message was not stored and acked")
	}
}

// More stored junk than the subscription limit does not hide an older message: the backlog is read backwards
// (item 4).
func TestBacklogReachesOlderMessages(t *testing.T) {
	oldSub, oldPage := SubscribeLimit, BacklogPage
	SubscribeLimit, BacklogPage = 30, 25
	defer func() { SubscribeLimit, BacklogPage = oldSub, oldPage }()
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bobSK := nostr.GeneratePrivateKey()
	bobPub, _ := nostr.GetPublicKey(bobSK)
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pool.Close()
	alice := nostr.GeneratePrivateKey()
	honest := wrapFor(t, alice, bobPub, "0001", proto.TypeChat, proto.Chat{Text: "older"}, time.Now().Add(-time.Hour))
	if ok, err := pool.Publish(ctx, []string{relay}, honest); len(ok) != 1 {
		t.Fatal(err)
	}
	for i := 0; i < 70; i++ { // newer junk addressed to bob
		j := &nostr.Event{Kind: giftwrap.KindWrap, CreatedAt: nostr.Timestamp(time.Now().Add(-time.Duration(i) * time.Second).Unix()), Tags: nostr.Tags{{"p", bobPub}}, Content: fmt.Sprint("junk", i)}
		_ = j.Sign(nostr.GeneratePrivateKey())
		if ok, err := pool.Publish(ctx, []string{relay}, j); len(ok) != 1 {
			t.Fatal(err)
		}
	}
	bob, err := New(Config{Secret: bobSK, Pool: pool, DB: openDB(t), Inbox: []string{relay}, Log: testutil.Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Message, 4)
	bob.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	bob.Start(ctx)
	if msg := recv(t, got); msg.OrderID != "0001" {
		t.Fatalf("got %+v", msg)
	}
}

// Acks are queued, not dropped, however many are due at once (item 9); a paused messenger keeps them queued.
func TestAcksQueuedNotDropped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bob := offline(t, openDB(t), nostr.GeneratePrivateKey(), nil)
	bob.HandleOther(func(context.Context, *Message) {})
	bob.SetPaused(true)
	bob.Start(ctx)
	alice := nostr.GeneratePrivateKey()
	for i := 0; i < 40; i++ {
		bob.receive(ctx, "wss://r", chat(t, alice, bob.Pub, "0001", fmt.Sprint(i)))
	}
	time.Sleep(200 * time.Millisecond)
	if n := bob.PendingAcks(); n != 40 {
		t.Fatalf("%d acks queued, want 40 (none dropped)", n)
	}
	bob.SetPaused(false)
	waitFor(t, ctx, func() bool { return bob.PendingAcks() == 0 })
}

// A handler past its timeout keeps its order busy until it returns, and the worker serves other orders meanwhile
// (item 9).
func TestTimedOutHandlerKeepsOrderBusy(t *testing.T) {
	oldT, oldW := HandlerTimeout, Workers
	HandlerTimeout, Workers = 300*time.Millisecond, 1
	defer func() { HandlerTimeout, Workers = oldT, oldW }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bob := offline(t, openDB(t), nostr.GeneratePrivateKey(), nil)
	release := make(chan struct{})
	got := make(chan string, 8)
	var running atomic.Int32
	bob.HandleOther(func(_ context.Context, msg *Message) {
		var c proto.Chat
		_ = msg.Decode(&c)
		if msg.OrderID == "aaaa" && running.Add(1) > 1 {
			t.Error("two handlers of one order at once")
		}
		defer func() {
			if msg.OrderID == "aaaa" {
				running.Add(-1)
			}
		}()
		if c.Text == "stuck" {
			<-release // ignores its context
		}
		got <- c.Text
	})
	bob.Start(ctx)
	alice := nostr.GeneratePrivateKey()
	bob.receive(ctx, "wss://r", chat(t, alice, bob.Pub, "aaaa", "stuck"))
	bob.receive(ctx, "wss://r", chat(t, alice, bob.Pub, "aaaa", "second of aaaa"))
	bob.receive(ctx, "wss://r", chat(t, alice, bob.Pub, "cccc", "other order"))
	select {
	case s := <-got:
		if s != "other order" {
			t.Fatalf("%q ran while the stuck handler of its order still ran", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker was not freed after the timeout")
	}
	select {
	case s := <-got:
		t.Fatalf("%q ran before the stuck handler returned", s)
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	for _, want := range []string{"stuck", "second of aaaa"} {
		select {
		case s := <-got:
			if s != want {
				t.Fatalf("got %q, want %q", s, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q not handled", want)
		}
	}
}

// Pausing (ending the Start context and SetPaused) stops receiving and sending; Start again resumes (item 11).
func TestPauseAndResume(t *testing.T) {
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice, _ := newMessenger(t, []string{relay})
	bob, bobGot := newMessenger(t, []string{relay})
	alice.Start(ctx)
	bctx, pause := context.WithCancel(ctx)
	bob.Start(bctx)
	time.Sleep(300 * time.Millisecond)
	pause()
	bob.SetPaused(true)
	inner, err := alice.Send(ctx, bob.Pub, "0001", proto.TypeChat, proto.Chat{Text: "while paused"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	quiet(t, bobGot, 800*time.Millisecond)
	// bob's own sending is held too
	if _, err := bob.Send(ctx, alice.Pub, "0002", proto.TypeChat, proto.Chat{Text: "held"}, nil); err != nil {
		t.Fatal(err)
	}
	if evs := bob.pool.Query(ctx, []string{relay}, nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {alice.Pub}}}); len(evs) != 0 {
		t.Fatal("a paused messenger published")
	}
	bob.SetPaused(false)
	bob.Start(ctx)
	if msg := recv(t, bobGot); msg.Inner.ID != inner.ID {
		t.Fatalf("got %+v", msg)
	}
	waitFor(t, ctx, func() bool { return alice.Acked(inner.ID) })
}

// The bucket map of the rate limiter never exceeds its cap (item 9).
func TestRateLimiterCap(t *testing.T) {
	old := maxBuckets
	maxBuckets = 10
	defer func() { maxBuckets = old }()
	r := newRateLimiter(30, 30)
	now := time.Now()
	for i := 0; i < 50; i++ {
		for j := 0; j < 30; j++ { // empty each bucket so that the sweep cannot free it
			r.allow(fmt.Sprint(i), now)
		}
	}
	if n := r.size(); n > 10 {
		t.Fatalf("%d buckets, cap 10", n)
	}
}

// Relay lists are filtered for usable relays before the first MaxRelays are taken; histories outlive T2 (item 9).
func TestRelayFilterAndRetention(t *testing.T) {
	pool := nostrnet.NewPoolWith(nostrnet.Options{Log: testutil.Logger(t)})
	t.Cleanup(pool.Close)
	m, err := New(Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: openDB(t), Log: testutil.Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for i := 0; i < MaxRelays; i++ {
		list = append(list, fmt.Sprintf("wss://10.0.0.%d", i+1))
	}
	list = append(list, "wss://relay-a.example", "https://not-a-relay.example", "wss://relay-b.example")
	if r := m.RelaysFor("x", list); len(r) != 2 || r[0] != "wss://relay-a.example" || r[1] != "wss://relay-b.example" {
		t.Fatalf("got %v", r)
	}
	if HistoryRetention < 120*24*time.Hour {
		t.Fatalf("history kept %v, less than the longest T2 (120 days)", HistoryRetention)
	}
}
