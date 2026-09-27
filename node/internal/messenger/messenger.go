// Package messenger delivers and receives the 1:1 messages of spec §4: it gift-wraps inner events, sends them to
// k inbox relays of the recipient, resends every 30 seconds until an ack arrives (for at most 7 days), acks and
// de-duplicates what it receives, and keeps outbox and inbox in the store.
package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

const (
	bucketOutbox = "outbox"
	bucketInbox  = "inbox"
	bucketWraps  = "wraps"
)

// Defaults of §4.2.
var (
	RetryInterval = 30 * time.Second
	RetryLimit    = 7 * 24 * time.Hour
)

// Message is a received, verified message.
type Message struct {
	Inner   *nostr.Event
	From    string
	Type    string
	OrderID string
	Relay   string
}

// Decode unmarshals the body.
func (m *Message) Decode(v any) error {
	if err := json.Unmarshal([]byte(m.Inner.Content), v); err != nil {
		return fmt.Errorf("%s body: %w", m.Type, err)
	}
	return nil
}

// Handler processes a received message.
type Handler func(ctx context.Context, m *Message)

// Resolver returns the inbox relays of a pubkey (10050), or nil.
type Resolver func(pubkey string) []string

type outboxEntry struct {
	Inner    *nostr.Event `json:"inner"`
	To       string       `json:"to"`
	Relays   []string     `json:"relays"`
	Created  int64        `json:"created"`
	LastSent int64        `json:"last_sent"`
	Sent     []string     `json:"sent,omitempty"` // relays that accepted the last wrap
	Acked    bool         `json:"acked"`
	AckedAt  int64        `json:"acked_at,omitempty"`
}

type inboxEntry struct {
	Inner    *nostr.Event `json:"inner"`
	Received int64        `json:"received"`
	Relay    string       `json:"relay"`
}

// Messenger is the mailbox of one identity.
type Messenger struct {
	secret, Pub string
	pool        *nostrnet.Pool
	db          *store.DB
	inbox       []string // our inbox relays
	defaults    []string // fallback relays for recipients without 10050
	k           int
	resolve     Resolver
	log         *slog.Logger

	mu       sync.Mutex
	handlers map[string]Handler
	fallback Handler
	queue    chan *Message
}

// Config configures a messenger.
type Config struct {
	Secret   string
	Pool     *nostrnet.Pool
	DB       *store.DB
	Inbox    []string // relays we read from
	Defaults []string // relays of recipients without inbox relays (usually = Inbox)
	K        int
	Resolve  Resolver
	Log      *slog.Logger
}

// New creates a messenger; Start begins receiving and resending.
func New(c Config) (*Messenger, error) {
	pub, err := nostr.GetPublicKey(c.Secret)
	if err != nil {
		return nil, fmt.Errorf("messenger key: %w", err)
	}
	if c.K <= 0 {
		c.K = 2
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Defaults == nil {
		c.Defaults = c.Inbox
	}
	return &Messenger{
		secret: c.Secret, Pub: pub, pool: c.Pool, db: c.DB, inbox: c.Inbox, defaults: c.Defaults, k: c.K,
		resolve: c.Resolve, log: c.Log.With("component", "messenger"),
		handlers: map[string]Handler{}, queue: make(chan *Message, 256),
	}, nil
}

// Handle registers the handler of a message type.
func (m *Messenger) Handle(typ string, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[typ] = h
}

// HandleOther registers the handler of all types without their own handler.
func (m *Messenger) HandleOther(h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = h
}

// Start subscribes to our inbox and runs the resend loop until ctx ends.
func (m *Messenger) Start(ctx context.Context) {
	go m.dispatch(ctx)
	filter := nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {m.Pub}}}
	m.pool.Subscribe(ctx, m.inbox, nostr.Filters{filter}, func(relay string, ev *nostr.Event) {
		m.receive(ctx, relay, ev)
	})
	go m.resendLoop(ctx)
}

// RelaysFor returns where to deliver messages for a pubkey: its inbox relays, else the hints, else our defaults.
func (m *Messenger) RelaysFor(pubkey string, hints []string) []string {
	if m.resolve != nil {
		if r := m.resolve(pubkey); len(r) > 0 {
			return r
		}
	}
	if len(hints) > 0 {
		return hints
	}
	return m.defaults
}

// Send signs, wraps and delivers a message. hints are relays named by the recipient (e.g. order.request relays).
// It returns the inner event (its id is what the recipient acks).
func (m *Messenger) Send(ctx context.Context, to, orderID, typ string, body any, hints []string) (*nostr.Event, error) {
	inner, err := giftwrap.NewInner(m.secret, to, orderID, typ, body, nostr.Now())
	if err != nil {
		return nil, err
	}
	if n := len(inner.String()); n > proto.MaxInnerBytes {
		return nil, fmt.Errorf("%s message is %d bytes, over the %d byte limit (§4.9)", typ, n, proto.MaxInnerBytes)
	}
	e := &outboxEntry{Inner: inner, To: to, Relays: m.RelaysFor(to, hints), Created: time.Now().Unix()}
	if typ == proto.TypeAck {
		// acks are never acked, so they are sent once and not kept
		m.deliver(ctx, e)
		return inner, nil
	}
	if err := m.db.Put(bucketOutbox, inner.ID, e); err != nil {
		return nil, fmt.Errorf("store outbox: %w", err)
	}
	m.deliver(ctx, e)
	return inner, nil
}

// deliver wraps the inner event anew and publishes it to the entry's relays.
func (m *Messenger) deliver(ctx context.Context, e *outboxEntry) {
	wrap, err := giftwrap.Wrap(m.secret, e.Inner, giftwrap.Options{})
	if err != nil {
		// wrapping only fails for messages that can never be sent; resending would not help
		m.log.Error("wrap failed, dropping the message", "id", e.Inner.ID, "type", giftwrap.Type(e.Inner), "err", err)
		_ = m.db.Delete(bucketOutbox, e.Inner.ID)
		return
	}
	ok, err := m.pool.Publish(ctx, e.Relays, wrap)
	need := min(m.k, len(e.Relays))
	if len(ok) < need {
		m.log.Warn("message reached too few relays", "type", giftwrap.Type(e.Inner), "to", e.To, "ok", len(ok), "need", need, "err", err)
	}
	now := time.Now().Unix()
	_ = store.Modify(m.db, bucketOutbox, e.Inner.ID, func(v *outboxEntry, exists bool) error {
		if !exists {
			return store.ErrStop
		}
		v.LastSent, v.Sent = now, ok
		return nil
	})
	m.log.Debug("sent", "type", giftwrap.Type(e.Inner), "to", e.To, "id", e.Inner.ID, "relays", ok)
}

func (m *Messenger) resendLoop(ctx context.Context) {
	t := time.NewTicker(RetryInterval / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var due []*outboxEntry
		_ = m.db.ForEach(bucketOutbox, func(_ string, raw []byte) error {
			var e outboxEntry
			if json.Unmarshal(raw, &e) != nil || e.Acked {
				return nil
			}
			if now.Sub(time.Unix(e.Created, 0)) > RetryLimit {
				return nil
			}
			if now.Sub(time.Unix(e.LastSent, 0)) >= RetryInterval {
				due = append(due, &e)
			}
			return nil
		})
		for _, e := range due {
			// the recipient may have published inbox relays since
			e.Relays = m.RelaysFor(e.To, e.Relays)
			m.deliver(ctx, e)
		}
	}
}

// Pending lists the unacknowledged messages (for status pages and tests).
func (m *Messenger) Pending() []*nostr.Event {
	var out []*nostr.Event
	_ = m.db.ForEach(bucketOutbox, func(_ string, raw []byte) error {
		var e outboxEntry
		if json.Unmarshal(raw, &e) == nil && !e.Acked {
			out = append(out, e.Inner)
		}
		return nil
	})
	return out
}

// Acked tells whether the recipient acknowledged an inner event.
func (m *Messenger) Acked(innerID string) bool {
	var e outboxEntry
	ok, _ := m.db.Get(bucketOutbox, innerID, &e)
	return ok && e.Acked
}

func (m *Messenger) receive(ctx context.Context, relay string, wrap *nostr.Event) {
	if m.db.Has(bucketWraps, wrap.ID) {
		return
	}
	inner, err := giftwrap.Unwrap(m.secret, wrap)
	_ = m.db.Put(bucketWraps, wrap.ID, time.Now().Unix())
	if err != nil {
		m.log.Debug("dropping wrap", "id", wrap.ID, "relay", relay, "err", err)
		return
	}
	typ := giftwrap.Type(inner)
	if typ != proto.TypeAck {
		// ack also duplicates: the sender resends until one of our acks arrives
		if _, err := m.Send(ctx, inner.PubKey, giftwrap.OrderID(inner), proto.TypeAck, proto.Ack{IDs: []string{inner.ID}}, nil); err != nil {
			m.log.Warn("ack failed", "id", inner.ID, "err", err)
		}
	}
	if m.db.Has(bucketInbox, inner.ID) {
		return
	}
	if err := m.db.Put(bucketInbox, inner.ID, inboxEntry{Inner: inner, Received: time.Now().Unix(), Relay: relay}); err != nil {
		if ctx.Err() == nil { // at shutdown the store closes before the subscriptions end
			m.log.Error("store inbox", "err", err)
		}
		return
	}
	msg := &Message{Inner: inner, From: inner.PubKey, Type: typ, OrderID: giftwrap.OrderID(inner), Relay: relay}
	if typ == proto.TypeAck {
		m.markAcked(msg)
		return
	}
	m.log.Info("received", "type", typ, "from", short(inner.PubKey), "order", msg.OrderID, "relay", relay)
	select {
	case m.queue <- msg:
	case <-ctx.Done():
	}
}

func (m *Messenger) markAcked(msg *Message) {
	var ack proto.Ack
	if msg.Decode(&ack) != nil {
		return
	}
	for _, id := range ack.IDs {
		_ = store.Modify(m.db, bucketOutbox, id, func(e *outboxEntry, exists bool) error {
			// only the recipient can acknowledge
			if !exists || e.Acked || e.To != msg.From {
				return store.ErrStop
			}
			e.Acked, e.AckedAt = true, time.Now().Unix()
			return nil
		})
	}
}

func (m *Messenger) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.queue:
			m.mu.Lock()
			h, ok := m.handlers[msg.Type]
			if !ok {
				h = m.fallback
			}
			m.mu.Unlock()
			if h == nil {
				m.log.Debug("no handler", "type", msg.Type)
				continue
			}
			h(ctx, msg)
		}
	}
}

// Inbox returns every stored received inner event of an order (all orders if orderID is empty).
func (m *Messenger) Inbox(orderID string) []*nostr.Event {
	var out []*nostr.Event
	_ = m.db.ForEach(bucketInbox, func(_ string, raw []byte) error {
		var e inboxEntry
		if json.Unmarshal(raw, &e) == nil && (orderID == "" || giftwrap.OrderID(e.Inner) == orderID) && giftwrap.Type(e.Inner) != proto.TypeAck {
			out = append(out, e.Inner)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b *nostr.Event) int { return int(a.CreatedAt - b.CreatedAt) })
	return out
}

// Outbox returns every sent inner event of an order except acks.
func (m *Messenger) Outbox(orderID string) []*nostr.Event {
	var out []*nostr.Event
	_ = m.db.ForEach(bucketOutbox, func(_ string, raw []byte) error {
		var e outboxEntry
		if json.Unmarshal(raw, &e) == nil && (orderID == "" || giftwrap.OrderID(e.Inner) == orderID) && giftwrap.Type(e.Inner) != proto.TypeAck {
			out = append(out, e.Inner)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b *nostr.Event) int { return int(a.CreatedAt - b.CreatedAt) })
	return out
}

func short(pk string) string {
	if len(pk) > 12 {
		return pk[:12]
	}
	return pk
}
