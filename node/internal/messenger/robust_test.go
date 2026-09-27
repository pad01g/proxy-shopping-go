package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// offline builds a messenger without relays (acks go nowhere) on the given store.
func offline(t *testing.T, db *store.DB, secret string, retry RetryFunc) *Messenger {
	t.Helper()
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	t.Cleanup(pool.Close)
	m, err := New(Config{Secret: secret, Pool: pool, DB: db, Retry: retry, Log: testutil.Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// wrapFor builds a gift-wrapped message from a sender secret to a recipient.
func wrapFor(t *testing.T, from, to, orderID, typ string, body any, at time.Time) *nostr.Event {
	t.Helper()
	inner, err := giftwrap.NewInner(from, to, orderID, typ, body, nostr.Timestamp(at.Unix()))
	if err != nil {
		t.Fatal(err)
	}
	w, err := giftwrap.Wrap(from, inner, giftwrap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func chat(t *testing.T, from, to, orderID, text string) *nostr.Event {
	return wrapFor(t, from, to, orderID, proto.TypeChat, proto.Chat{Text: text}, time.Now())
}

func recv(t *testing.T, ch <-chan *Message) *Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("message not handled")
		return nil
	}
}

func quiet(t *testing.T, ch <-chan *Message, d time.Duration) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message %s", msg.Inner.Content)
	case <-time.After(d):
	}
}

// A message received but not handled when the node stops is handled at the next start (it used to be stored as
// seen first and then lost).
func TestUnhandledMessageSurvivesRestart(t *testing.T) {
	db := openDB(t)
	bobSK, aliceSK := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	ctx, cancel := context.WithCancel(context.Background())
	bob := offline(t, db, bobSK, nil)
	entered := make(chan struct{}, 1)
	bob.HandleOther(func(hctx context.Context, _ *Message) {
		entered <- struct{}{}
		<-hctx.Done() // the node stops while the handler works
	})
	bob.Start(ctx)
	w := chat(t, aliceSK, bob.Pub, "0001", "funded")
	bob.receive(ctx, "wss://r", w)
	<-entered
	cancel()
	time.Sleep(50 * time.Millisecond)

	// restart on the same store
	again := offline(t, db, bobSK, nil)
	got := make(chan *Message, 4)
	again.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	again.Start(ctx2)
	if msg := recv(t, got); msg.OrderID != "0001" {
		t.Fatalf("got %+v", msg)
	}
	waitFor(t, ctx2, func() bool { return !pending(db, w) })
	// handled now: a third start does not repeat it, nor does the same wrap arriving again
	third := offline(t, db, bobSK, nil)
	got3 := make(chan *Message, 4)
	third.HandleOther(func(_ context.Context, msg *Message) { got3 <- msg })
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	third.Start(ctx3)
	third.receive(ctx3, "wss://r", w)
	quiet(t, got3, 300*time.Millisecond)
}

func TestHandlerPanicIsRecovered(t *testing.T) {
	db := openDB(t)
	aliceSK := nostr.GeneratePrivateKey()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bob := offline(t, db, nostr.GeneratePrivateKey(), nil)
	got := make(chan *Message, 4)
	bob.Handle(proto.TypeOrderCancel, func(context.Context, *Message) { panic("boom") })
	bob.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	bob.Start(ctx)
	bob.receive(ctx, "wss://r", wrapFor(t, aliceSK, bob.Pub, "0001", proto.TypeOrderCancel, proto.OrderCancel{Reason: "x"}, time.Now()))
	bob.receive(ctx, "wss://r", chat(t, aliceSK, bob.Pub, "0001", "after the panic"))
	if msg := recv(t, got); msg.Type != proto.TypeChat {
		t.Fatalf("got %s", msg.Type)
	}
	// the panicking message counts as handled (running it again would panic again)
	waitFor(t, ctx, func() bool {
		for _, ev := range bob.Inbox("0001") {
			var e inboxEntry
			if ok, _ := db.Get(bucketInbox, ev.ID, &e); !ok || e.Pending {
				return false
			}
		}
		return len(bob.Inbox("0001")) == 2
	})
}

// A stuck handler holds up its own order only; messages of one order run one at a time; handlers get a deadline.
func TestDispatchPerOrder(t *testing.T) {
	old := HandlerTimeout
	HandlerTimeout = 3 * time.Second
	defer func() { HandlerTimeout = old }()
	db := openDB(t)
	aliceSK := nostr.GeneratePrivateKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bob := offline(t, db, nostr.GeneratePrivateKey(), nil)
	got := make(chan *Message, 16)
	release := make(chan struct{})
	var running, maxRunning atomic.Int32
	var mu sync.Mutex
	var order []string
	bob.HandleOther(func(hctx context.Context, msg *Message) {
		if _, ok := hctx.Deadline(); !ok {
			t.Error("handler context without deadline")
		}
		var c proto.Chat
		_ = msg.Decode(&c)
		if c.Text == "stuck" {
			<-release // ignores its context on purpose
			return
		}
		if msg.OrderID == "bbbb" {
			n := running.Add(1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			running.Add(-1)
			mu.Lock()
			order = append(order, c.Text)
			mu.Unlock()
		}
		got <- msg
	})
	defer close(release)
	bob.Start(ctx)
	bob.receive(ctx, "wss://r", chat(t, aliceSK, bob.Pub, "aaaa", "stuck"))
	bob.receive(ctx, "wss://r", chat(t, aliceSK, bob.Pub, "aaaa", "behind the stuck one"))
	for _, s := range []string{"1", "2", "3"} {
		bob.receive(ctx, "wss://r", chat(t, aliceSK, bob.Pub, "bbbb", s))
	}
	for i := 0; i < 3; i++ {
		if msg := recv(t, got); msg.OrderID != "bbbb" {
			t.Fatalf("order aaaa ran before the stuck handler timed out: %+v", msg)
		}
	}
	if maxRunning.Load() != 1 || order[0] != "1" || order[1] != "2" || order[2] != "3" {
		t.Fatalf("messages of one order ran concurrently (%d) or out of order %v", maxRunning.Load(), order)
	}
	// after the timeout the next message of the stuck order runs
	if msg := recv(t, got); msg.OrderID != "aaaa" {
		t.Fatalf("got %+v", msg)
	}
}

func TestSenderRateLimit(t *testing.T) {
	oldRate := SenderRate
	SenderRate = 1 // per minute: no refill worth speaking of while the test runs
	defer func() { SenderRate = oldRate }()
	db := openDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bob := offline(t, db, nostr.GeneratePrivateKey(), nil)
	var n atomic.Int32
	bob.HandleOther(func(context.Context, *Message) { n.Add(1) })
	bob.Start(ctx)
	spammer, friend := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	var dropped *nostr.Event
	for i := 0; i < SenderBurst+10; i++ {
		w := chat(t, spammer, bob.Pub, "", fmt.Sprint("spam ", i))
		bob.receive(ctx, "wss://r", w)
		dropped = w
	}
	bob.receive(ctx, "wss://r", chat(t, friend, bob.Pub, "", "hello"))
	waitFor(t, ctx, func() bool { return n.Load() >= int32(SenderBurst)+1 })
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != int32(SenderBurst)+1 {
		t.Fatalf("handled %d, want %d", got, SenderBurst+1)
	}
	// a dropped wrap is not remembered as seen, so it gets through later
	if bob.db.Has(bucketWraps, dropped.ID) {
		t.Fatal("dropped wrap marked as seen")
	}
	r := newRateLimiter(30, 30)
	now := time.Now()
	for i := 0; i < 30; i++ {
		r.allow("a", now)
	}
	if r.allow("a", now) || !r.allow("a", now.Add(2*time.Second)) {
		t.Fatal("token bucket does not refill at 30 per minute")
	}
}

func TestRelayLimits(t *testing.T) {
	db := openDB(t)
	m := offline(t, db, nostr.GeneratePrivateKey(), nil)
	var hints []string
	for i := 0; i < 20; i++ {
		hints = append(hints, "wss://r"+string(rune('a'+i)))
	}
	if r := m.RelaysFor("x", hints); len(r) != MaxRelays || r[0] != "wss://ra" {
		t.Fatalf("hints not capped: %v", r)
	}
	m.resolve = func(string) []string { return append(hints[:2:2], hints[:2]...) }
	if r := m.RelaysFor("x", nil); len(r) != 2 {
		t.Fatalf("duplicates kept: %v", r)
	}

	// with k = 2 only two of three live relays get the wrap
	relays := []string{testutil.StartRelay(t), testutil.StartRelay(t), testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pool.Close()
	alice, err := New(Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: openDB(t), K: 2, Log: testutil.Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	bob := nostr.GeneratePrivateKey()
	bobPub, _ := nostr.GetPublicKey(bob)
	// a dead relay first: the next one is tried instead
	if _, err := alice.Send(ctx, bobPub, "", proto.TypeChat, proto.Chat{Text: "hi"}, append([]string{"ws://127.0.0.1:1"}, relays...)); err != nil {
		t.Fatal(err)
	}
	reached := 0
	for _, r := range relays {
		if len(pool.Query(ctx, []string{r}, nostr.Filter{Kinds: []int{giftwrap.KindWrap}})) == 1 {
			reached++
		}
	}
	if reached != 2 {
		t.Fatalf("wrap on %d relays, want k = 2", reached)
	}
}

// Resends reuse the wrap; recipients the engines did not mark are not resent to; expired entries are not pending.
func TestResendPolicy(t *testing.T) {
	old := RetryInterval
	RetryInterval = 300 * time.Millisecond
	defer func() { RetryInterval = old }()
	relay := testutil.StartRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	defer pool.Close()
	stranger := nostr.GeneratePrivateKey()
	strangerPub, _ := nostr.GetPublicKey(stranger)
	db := openDB(t)
	alice, err := New(Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: db, Inbox: []string{relay}, K: 2, Log: testutil.Logger(t),
		Retry: func(to, orderID string) bool { return orderID == "0001" }})
	if err != nil {
		t.Fatal(err)
	}
	alice.Start(ctx)
	bobPub, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	party, _ := alice.Send(ctx, bobPub, "0001", proto.TypeChat, proto.Chat{Text: "party"}, nil)
	other, _ := alice.Send(ctx, strangerPub, "0002", proto.TypeChat, proto.Chat{Text: "no order"}, nil)
	var first, firstOther outboxEntry
	_, _ = db.Get(bucketOutbox, party.ID, &first)
	_, _ = db.Get(bucketOutbox, other.ID, &firstOther)
	var now, nowOther outboxEntry
	waitFor(t, ctx, func() bool {
		_, _ = db.Get(bucketOutbox, party.ID, &now)
		return now.LastSent > first.LastSent
	})
	_, _ = db.Get(bucketOutbox, other.ID, &nowOther)
	if nowOther.LastSent != firstOther.LastSent {
		t.Fatal("message to a recipient without an order was resent")
	}
	// resends reuse the wrap except on resends 1, 2, 4, 8, …: the relay holds far fewer wraps than sendings
	waitFor(t, ctx, func() bool {
		_, _ = db.Get(bucketOutbox, party.ID, &now)
		return now.Resends >= 5
	})
	_, _ = db.Get(bucketOutbox, party.ID, &now)
	wraps := pool.Query(ctx, []string{relay}, nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {bobPub}}})
	if want := 1 + bits.Len(uint(now.Resends)); len(wraps) != want && len(wraps) != want+1 { // +1: a resend in flight
		t.Fatalf("relay stores %d wraps after %d resends, want %d", len(wraps), now.Resends, want)
	}
	for i, want := range []bool{false, true, true, false, true, false, false, false, true} {
		if rewrap(i) != want {
			t.Fatalf("rewrap(%d)", i)
		}
	}
	if len(alice.Pending()) != 2 {
		t.Fatalf("pending %d", len(alice.Pending()))
	}
	alice.outMu.Lock()
	alice.outbox[other.ID].created = time.Now().Add(-RetryLimit - time.Hour).Unix()
	alice.outMu.Unlock()
	if p := alice.Pending(); len(p) != 1 || p[0].ID != party.ID {
		t.Fatalf("expired message still pending: %d", len(p))
	}
}

func TestPrune(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := offline(t, db, nostr.GeneratePrivateKey(), nil)
	m.HandleOther(func(context.Context, *Message) {})
	m.Start(ctx)
	aliceSK := nostr.GeneratePrivateKey()

	// a wrap older than WrapMaxAge is ignored
	oldWrap := wrapFor(t, aliceSK, m.Pub, "0001", proto.TypeChat, proto.Chat{Text: "old"}, time.Now().Add(-WrapMaxAge-time.Hour))
	m.receive(ctx, "wss://r", oldWrap)
	if len(m.Inbox("")) != 0 {
		t.Fatal("stale wrap accepted")
	}
	w := chat(t, aliceSK, m.Pub, "0001", "hi")
	m.receive(ctx, "wss://r", w)
	time.Sleep(100 * time.Millisecond)
	sent, _ := m.Send(ctx, m.Pub, "0001", proto.TypeChat, proto.Chat{Text: "x"}, nil)
	unacked, _ := m.Send(ctx, m.Pub, "0002", proto.TypeChat, proto.Chat{Text: "y"}, nil)
	m.outMu.Lock()
	m.outbox[sent.ID].acked = true
	m.outMu.Unlock()

	if n := m.prune(time.Now()); n != 0 {
		t.Fatalf("pruned %d fresh entries", n)
	}
	later := time.Now().Add(HistoryRetention + time.Hour)
	if n := m.prune(later); n != 4 { // the wrap id, the inbox entry, the acked and the expired message
		t.Fatalf("pruned %d, want 4", n)
	}
	if m.db.Has(bucketWraps, w.ID) || len(m.Inbox("")) != 0 || len(m.Outbox("")) != 0 || m.Acked(sent.ID) || m.Acked(unacked.ID) {
		t.Fatal("entries left after prune")
	}
}

// pending tells whether the message of a wrap is stored and not yet handled.
func pending(db *store.DB, w *nostr.Event) bool {
	var found bool
	_ = db.ForEach(bucketInbox, func(_ string, raw []byte) error {
		var e inboxEntry
		if json.Unmarshal(raw, &e) == nil && e.Pending {
			found = true
		}
		return nil
	})
	return found
}
