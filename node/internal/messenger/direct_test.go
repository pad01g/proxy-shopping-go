package messenger

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// §4.2: P2P first; the mailbox when P2P does not take the wrap (or no address is known). Acks go the same way.
// A wrap that comes both ways is handled once (de-duplicated by the inner id).
func TestDirectFirstThenMailbox(t *testing.T) {
	relays := []string{testutil.StartRelay(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice, _ := newMessenger(t, relays)
	bob, bobGot := newMessenger(t, relays)
	var up atomic.Bool
	var calls atomic.Int32
	link := func(to *Messenger) DirectFunc {
		return func(ctx context.Context, pk, _ string, wrap *nostr.Event) (bool, error) {
			calls.Add(1)
			if pk != to.Pub {
				return false, nil // address unknown
			}
			if !up.Load() {
				return false, errors.New("dial failed")
			}
			if err := to.ReceiveDirect(wrap); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	alice.direct, bob.direct = link(bob), link(alice)
	if err := bob.ReceiveDirect(chat(t, alice.secret, bob.Pub, "0001", "early")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("receiving before Start: %v", err)
	}
	alice.Start(ctx)
	bob.Start(ctx)
	time.Sleep(300 * time.Millisecond)

	// over P2P: nothing reaches the relay, the ack comes back over P2P too
	up.Store(true)
	inner, err := alice.Send(ctx, bob.Pub, "000102030405060708090a0b0c0d0e0f", proto.TypeChat, proto.Chat{Text: "p2p"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg := recv(t, bobGot); msg.Inner.ID != inner.ID || msg.Relay != ViaP2P {
		t.Fatalf("got %+v", msg)
	}
	waitFor(t, ctx, func() bool { return alice.Acked(inner.ID) })
	if v := alice.DeliveredVia(inner.ID); v != ViaP2P {
		t.Fatalf("delivered via %q", v)
	}
	if evs := alice.pool.Query(ctx, relays, nostr.Filter{Kinds: []int{giftwrap.KindWrap}}); len(evs) != 0 {
		t.Fatalf("relay got %d wraps although P2P delivered", len(evs))
	}

	// P2P down: the mailbox
	up.Store(false)
	before := calls.Load()
	inner2, err := alice.Send(ctx, bob.Pub, "000102030405060708090a0b0c0d0e0f", proto.TypeChat, proto.Chat{Text: "mailbox"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg := recv(t, bobGot); msg.Inner.ID != inner2.ID || msg.Relay == ViaP2P {
		t.Fatalf("got %+v", msg)
	}
	if calls.Load() == before {
		t.Fatal("P2P was not tried first")
	}
	waitFor(t, ctx, func() bool { return alice.Acked(inner2.ID) })
	if v := alice.DeliveredVia(inner2.ID); v != "nostr" {
		t.Fatalf("delivered via %q", v)
	}

	// the same message by both paths: handled once, the P2P copy is still answered ok
	w := chat(t, alice.secret, bob.Pub, "0002", "twice")
	if err := bob.ReceiveDirect(w); err != nil {
		t.Fatal(err)
	}
	recv(t, bobGot)
	if err := bob.ReceiveDirect(w); err != nil {
		t.Fatalf("duplicate answered %v", err)
	}
	if _, err := alice.pool.Publish(ctx, relays, w); err != nil {
		t.Fatal(err)
	}
	quiet(t, bobGot, 700*time.Millisecond)

	// what is not for us, or broken, is refused
	if err := bob.ReceiveDirect(chat(t, alice.secret, alice.Pub, "0003", "not bob's")); !errors.Is(err, ErrUnwrap) {
		t.Fatalf("wrap for another key: %v", err)
	}
	bad := *chat(t, alice.secret, bob.Pub, "0003", "x")
	bad.Content += "x"
	if err := bob.ReceiveDirect(&bad); !errors.Is(err, ErrInvalidWrap) {
		t.Fatalf("tampered wrap: %v", err)
	}
	old := wrapFor(t, alice.secret, bob.Pub, "0003", proto.TypeChat, proto.Chat{Text: "old"}, time.Now().Add(-WrapMaxAge-48*time.Hour))
	if err := bob.ReceiveDirect(old); err == nil {
		t.Fatal("expired wrap taken")
	}
}
