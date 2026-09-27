package messenger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

func newMessenger(t *testing.T, relays []string) (*Messenger, chan *Message) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pool := nostrnet.NewPool(nil, testutil.Logger(t))
	t.Cleanup(pool.Close)
	m, err := New(Config{Secret: nostr.GeneratePrivateKey(), Pool: pool, DB: db, Inbox: relays, K: 2, Log: testutil.Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Message, 16)
	m.HandleOther(func(_ context.Context, msg *Message) { got <- msg })
	return m, got
}

func TestSendAckDedupe(t *testing.T) {
	relays := []string{testutil.StartRelay(t), testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	alice, _ := newMessenger(t, relays)
	bob, bobGot := newMessenger(t, relays)
	alice.Start(ctx)
	bob.Start(ctx)
	time.Sleep(300 * time.Millisecond) // let the subscriptions open

	inner, err := alice.Send(ctx, bob.Pub, "000102030405060708090a0b0c0d0e0f", proto.TypeChat, proto.Chat{Text: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-bobGot:
		var c proto.Chat
		if msg.From != alice.Pub || msg.Decode(&c) != nil || c.Text != "hi" || msg.OrderID == "" {
			t.Fatalf("got %+v", msg)
		}
	case <-ctx.Done():
		t.Fatal("message not delivered")
	}
	// the message arrives through both relays but is handled once
	select {
	case msg := <-bobGot:
		t.Fatalf("duplicate delivery %+v", msg)
	case <-time.After(700 * time.Millisecond):
	}
	waitFor(t, ctx, func() bool { return alice.Acked(inner.ID) })
	if len(alice.Pending()) != 0 {
		t.Fatal("acked message still pending")
	}
	if len(bob.Inbox("000102030405060708090a0b0c0d0e0f")) != 1 || len(alice.Outbox("")) != 1 {
		t.Fatal("inbox / outbox history")
	}
}

func TestResendUntilAck(t *testing.T) {
	old := RetryInterval
	RetryInterval = 600 * time.Millisecond
	defer func() { RetryInterval = old }()

	relays := []string{testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice, _ := newMessenger(t, relays)
	alice.Start(ctx)
	bob, bobGot := newMessenger(t, relays)

	// bob is offline while alice sends
	inner, err := alice.Send(ctx, bob.Pub, "", proto.TypeChat, proto.Chat{Text: "later"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if alice.Acked(inner.ID) {
		t.Fatal("acked without recipient")
	}
	bob.Start(ctx)
	select {
	case <-bobGot:
	case <-ctx.Done():
		t.Fatal("not delivered after coming online")
	}
	select {
	case msg := <-bobGot:
		t.Fatalf("resent copy handled again: %+v", msg)
	case <-time.After(1500 * time.Millisecond):
	}
	waitFor(t, ctx, func() bool { return alice.Acked(inner.ID) })
}

func waitFor(t *testing.T, ctx context.Context, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
