// Package messenger delivers and receives the 1:1 messages of spec §4: it gift-wraps inner events, sends them to
// k inbox relays of the recipient, resends every 30 seconds until an ack arrives (for at most 7 days), acks and
// de-duplicates what it receives, and keeps outbox and inbox in the store.
//
// A received message is stored (not yet handled) before it is acked and before its handler runs, and marked
// handled when the handler returns; messages left unhandled by a crash are handled again at the next Start.
// Handlers run in a bounded pool of workers, one message of an order at a time, with a timeout.
package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
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

// Defaults of §4.2 and §4.10, and the limits of this implementation.
var (
	RetryInterval = 30 * time.Second
	RetryLimit    = 7 * 24 * time.Hour

	// MaxRelays is how many inbox relays of a recipient (10050 or hints) are used (§4.10).
	MaxRelays = 8
	// HandlerTimeout bounds one handler call; the handler's context ends then.
	HandlerTimeout = 2 * time.Minute
	// Workers is how many handlers run at once (messages of one order never run concurrently).
	Workers = 8
	// SenderRate and SenderBurst limit the messages accepted from one sender per minute. Messages over the limit
	// are dropped without an ack, so an honest sender delivers them with a later resend.
	SenderRate  = 30
	SenderBurst = 30

	// WrapMaxAge: older wraps are ignored and their ids forgotten (a resend stops after RetryLimit anyway).
	WrapMaxAge = 14 * 24 * time.Hour
	// HistoryRetention keeps handled inbox and finished outbox entries of an order (disputes read them as
	// evidence until T2, which is about 40 days by default); OtherRetention is for messages without an order.
	HistoryRetention = 60 * 24 * time.Hour
	OtherRetention   = 30 * 24 * time.Hour
	// PruneInterval is how often old entries are removed.
	PruneInterval = time.Hour
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

// Handler processes a received message. ctx ends after HandlerTimeout.
type Handler func(ctx context.Context, m *Message)

// Resolver returns the inbox relays of a pubkey (10050), or nil.
type Resolver func(pubkey string) []string

// RetryFunc tells whether unacknowledged messages to a recipient about an order are resent (§4.10: replies to
// somebody without an order with us are sent once).
type RetryFunc func(to, orderID string) bool

type outboxEntry struct {
	Inner    *nostr.Event `json:"inner"`
	Wrap     *nostr.Event `json:"wrap,omitempty"` // resent as is, except on resends 1, 2, 4, 8, … (see rewrap)
	Resends  int          `json:"resends,omitempty"`
	To       string       `json:"to"`
	Relays   []string     `json:"relays"`
	Created  int64        `json:"created"`
	LastSent int64        `json:"last_sent"`
	Sent     []string     `json:"sent,omitempty"` // relays that accepted the last sending
	Acked    bool         `json:"acked"`
	AckedAt  int64        `json:"acked_at,omitempty"`
}

type inboxEntry struct {
	Inner    *nostr.Event `json:"inner"`
	Received int64        `json:"received"`
	Relay    string       `json:"relay"`
	// Pending is set until the handler returned. Entries written before this field existed read as handled.
	Pending bool  `json:"pending,omitempty"`
	Handled int64 `json:"handled,omitempty"`
}

// outMeta is the in-memory index of the outbox, so that the resend loop does not read the whole bucket.
type outMeta struct {
	to, orderID string
	created     int64
	lastSent    int64
	acked       bool
	ackedAt     int64
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
	retry       RetryFunc
	log         *slog.Logger

	mu       sync.Mutex
	handlers map[string]Handler
	fallback Handler

	outMu  sync.Mutex
	outbox map[string]*outMeta

	// the package defaults at New
	retryInterval, retryLimit, handlerTimeout time.Duration

	disp    *dispatcher
	senders *rateLimiter
	acks    chan struct{} // bounds concurrent ack sending
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
	Retry    RetryFunc // nil: resend everything
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
	m := &Messenger{
		secret: c.Secret, Pub: pub, pool: c.Pool, db: c.DB, inbox: c.Inbox, defaults: c.Defaults, k: c.K,
		resolve: c.Resolve, retry: c.Retry, log: c.Log.With("component", "messenger"),
		handlers: map[string]Handler{}, outbox: map[string]*outMeta{},
		senders: newRateLimiter(SenderRate, SenderBurst), acks: make(chan struct{}, 16),
		retryInterval: RetryInterval, retryLimit: RetryLimit, handlerTimeout: HandlerTimeout,
	}
	m.disp = newDispatcher(m)
	if err := m.loadOutbox(); err != nil {
		return nil, err
	}
	return m, nil
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

func (m *Messenger) handler(typ string) Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.handlers[typ]; ok {
		return h
	}
	return m.fallback
}

// Start handles what a previous run left unhandled, subscribes to our inbox and runs the resend loop until
// ctx ends. Register the handlers before.
func (m *Messenger) Start(ctx context.Context) {
	m.disp.start(ctx)
	m.redispatch()
	filter := nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {m.Pub}}, Limit: 1000}
	m.pool.Subscribe(ctx, m.inbox, nostr.Filters{filter}, func(relay string, ev *nostr.Event) {
		m.receive(ctx, relay, ev)
	})
	go m.resendLoop(ctx)
	go m.pruneLoop(ctx)
}

// RelaysFor returns where to deliver messages for a pubkey: its inbox relays, else the hints, else our
// defaults; at most MaxRelays of them.
func (m *Messenger) RelaysFor(pubkey string, hints []string) []string {
	var r []string
	if m.resolve != nil {
		r = m.resolve(pubkey)
	}
	if len(r) == 0 {
		r = hints
	}
	if len(r) == 0 {
		r = m.defaults
	}
	return capRelays(r)
}

func capRelays(r []string) []string {
	out := make([]string, 0, min(len(r), MaxRelays))
	for _, u := range r {
		if len(out) == MaxRelays {
			break
		}
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
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
	wrap, err := giftwrap.Wrap(m.secret, inner, giftwrap.Options{})
	if err != nil {
		return nil, err
	}
	e := &outboxEntry{Inner: inner, Wrap: wrap, To: to, Relays: m.RelaysFor(to, hints), Created: time.Now().Unix()}
	if typ == proto.TypeAck {
		// acks are never acked, so they are sent once and not kept
		m.deliver(ctx, e)
		return inner, nil
	}
	if err := m.db.Put(bucketOutbox, inner.ID, e); err != nil {
		return nil, fmt.Errorf("store outbox: %w", err)
	}
	m.outMu.Lock()
	m.outbox[inner.ID] = &outMeta{to: to, orderID: orderID, created: e.Created}
	m.outMu.Unlock()
	m.deliver(ctx, e)
	return inner, nil
}

// deliver publishes the entry's wrap until k relays accepted it, trying the further relays of the list only
// when some of the first ones fail (§4.2: k relays, or all when there are fewer; §4.10: not more than k).
func (m *Messenger) deliver(ctx context.Context, e *outboxEntry) {
	if e.Wrap == nil { // entries of older versions
		wrap, err := giftwrap.Wrap(m.secret, e.Inner, giftwrap.Options{})
		if err != nil {
			m.log.Error("wrap failed, dropping the message", "id", e.Inner.ID, "type", giftwrap.Type(e.Inner), "err", err)
			_ = m.db.Delete(bucketOutbox, e.Inner.ID)
			m.outMu.Lock()
			delete(m.outbox, e.Inner.ID)
			m.outMu.Unlock()
			return
		}
		e.Wrap = wrap
	}
	need := min(m.k, len(e.Relays))
	var ok []string
	var errs []error
	rest := e.Relays
	for len(ok) < need && len(rest) > 0 {
		n := min(need-len(ok), len(rest))
		got, err := m.pool.Publish(ctx, rest[:n], e.Wrap)
		ok, rest = append(ok, got...), rest[n:]
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(ok) < need {
		m.log.Warn("message reached too few relays", "type", giftwrap.Type(e.Inner), "to", e.To, "ok", len(ok), "need", need, "errs", errs)
	}
	now := time.Now().Unix()
	if giftwrap.Type(e.Inner) == proto.TypeAck {
		return
	}
	var stored bool
	_ = store.Modify(m.db, bucketOutbox, e.Inner.ID, func(v *outboxEntry, exists bool) error {
		if !exists {
			return store.ErrStop
		}
		stored = true
		v.LastSent, v.Sent, v.Wrap, v.Relays, v.Resends = now, ok, e.Wrap, e.Relays, e.Resends
		return nil
	})
	if stored {
		m.outMu.Lock()
		if meta := m.outbox[e.Inner.ID]; meta != nil {
			meta.lastSent = now
		}
		m.outMu.Unlock()
	}
	m.log.Debug("sent", "type", giftwrap.Type(e.Inner), "to", e.To, "id", e.Inner.ID, "relays", ok)
}

func (m *Messenger) loadOutbox() error {
	return m.db.ForEach(bucketOutbox, func(id string, raw []byte) error {
		var e outboxEntry
		if json.Unmarshal(raw, &e) != nil || e.Inner == nil {
			return nil
		}
		m.outbox[id] = &outMeta{
			to: e.To, orderID: giftwrap.OrderID(e.Inner), created: e.Created, lastSent: e.LastSent,
			acked: e.Acked, ackedAt: e.AckedAt,
		}
		return nil
	})
}

func (m *Messenger) expired(meta *outMeta, now time.Time) bool {
	return now.Sub(time.Unix(meta.created, 0)) > m.retryLimit
}

// due lists the outbox entries to resend now.
func (m *Messenger) due(now time.Time) []string {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	var ids []string
	for id, meta := range m.outbox {
		if meta.acked || m.expired(meta, now) || now.Sub(time.Unix(meta.lastSent, 0)) < m.retryInterval {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// rewrap tells whether a resend gets a new wrap. A relay does not pass an event it already has to live
// subscribers again, so resending the same wrap only helps relays that missed it; a recipient that was online
// and still missed the message (or dropped it over its rate limit) needs a new event. New wraps on resends
// 1, 2, 4, 8, … recover quickly and keep what relays store small (about 16 wraps over the 7 days).
func rewrap(resend int) bool { return resend > 0 && resend&(resend-1) == 0 }

func (m *Messenger) resendLoop(ctx context.Context) {
	t := time.NewTicker(m.retryInterval / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, id := range m.due(time.Now()) {
			var e outboxEntry
			if ok, err := m.db.Get(bucketOutbox, id, &e); !ok || err != nil || e.Acked {
				continue
			}
			if m.retry != nil && !m.retry(e.To, giftwrap.OrderID(e.Inner)) {
				// not somebody we have an order with: the first sending was all (§4.10)
				m.outMu.Lock()
				if meta := m.outbox[id]; meta != nil {
					meta.lastSent = time.Now().Unix()
				}
				m.outMu.Unlock()
				continue
			}
			// the recipient may have published inbox relays since
			e.Relays = m.RelaysFor(e.To, e.Relays)
			e.Resends++
			if rewrap(e.Resends) {
				e.Wrap = nil
			}
			m.deliver(ctx, &e)
		}
	}
}

// Pending lists the unacknowledged messages that are still being resent (for status pages and tests).
func (m *Messenger) Pending() []*nostr.Event {
	now := time.Now()
	var ids []string
	m.outMu.Lock()
	for id, meta := range m.outbox {
		if !meta.acked && !m.expired(meta, now) {
			ids = append(ids, id)
		}
	}
	m.outMu.Unlock()
	var out []*nostr.Event
	for _, id := range ids {
		var e outboxEntry
		if ok, _ := m.db.Get(bucketOutbox, id, &e); ok && e.Inner != nil {
			out = append(out, e.Inner)
		}
	}
	return out
}

// Acked tells whether the recipient acknowledged an inner event.
func (m *Messenger) Acked(innerID string) bool {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	meta := m.outbox[innerID]
	return meta != nil && meta.acked
}

func (m *Messenger) receive(ctx context.Context, relay string, wrap *nostr.Event) {
	if time.Since(wrap.CreatedAt.Time()) > WrapMaxAge || m.db.Has(bucketWraps, wrap.ID) {
		return
	}
	inner, err := giftwrap.Unwrap(m.secret, wrap)
	if err != nil {
		_ = m.db.Put(bucketWraps, wrap.ID, time.Now().Unix())
		m.log.Debug("dropping wrap", "id", wrap.ID, "relay", relay, "err", err)
		return
	}
	typ := giftwrap.Type(inner)
	msg := &Message{Inner: inner, From: inner.PubKey, Type: typ, OrderID: giftwrap.OrderID(inner), Relay: relay}
	if typ == proto.TypeAck {
		_ = m.db.Put(bucketWraps, wrap.ID, time.Now().Unix())
		m.markAcked(msg)
		return
	}
	if !m.senders.allow(inner.PubKey, time.Now()) {
		// not remembered as seen: the sender's resend (or this wrap at our next reconnect) gets another chance
		m.log.Debug("sender over the rate limit, dropping", "from", short(inner.PubKey), "type", typ)
		return
	}
	// store before acking and before handling, so that neither a crash nor a full queue loses it
	fresh := false
	err = store.Modify(m.db, bucketInbox, inner.ID, func(v *inboxEntry, exists bool) error {
		if exists {
			return store.ErrStop
		}
		fresh = true
		*v = inboxEntry{Inner: inner, Received: time.Now().Unix(), Relay: relay, Pending: true}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil { // at shutdown the store closes before the subscriptions end
			m.log.Error("store inbox", "err", err)
		}
		return
	}
	_ = m.db.Put(bucketWraps, wrap.ID, time.Now().Unix())
	// ack duplicates too: the sender resends until one of our acks arrives
	m.ack(ctx, inner)
	if !fresh {
		return
	}
	m.log.Info("received", "type", typ, "from", short(inner.PubKey), "order", msg.OrderID, "relay", relay)
	m.disp.enqueue(msg)
}

// ack sends the ack in the background; when too many are in flight it is skipped (the sender resends).
func (m *Messenger) ack(ctx context.Context, inner *nostr.Event) {
	select {
	case m.acks <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-m.acks }()
		if _, err := m.Send(ctx, inner.PubKey, giftwrap.OrderID(inner), proto.TypeAck, proto.Ack{IDs: []string{inner.ID}}, nil); err != nil {
			m.log.Warn("ack failed", "id", inner.ID, "err", err)
		}
	}()
}

func (m *Messenger) markAcked(msg *Message) {
	var ack proto.Ack
	if msg.Decode(&ack) != nil {
		return
	}
	for _, id := range ack.IDs {
		m.outMu.Lock()
		meta := m.outbox[id]
		// only the recipient can acknowledge
		known := meta != nil && !meta.acked && meta.to == msg.From
		m.outMu.Unlock()
		if !known {
			continue
		}
		now := time.Now().Unix()
		_ = store.Modify(m.db, bucketOutbox, id, func(e *outboxEntry, exists bool) error {
			if !exists || e.Acked || e.To != msg.From {
				return store.ErrStop
			}
			e.Acked, e.AckedAt = true, now
			e.Wrap = nil // not needed any more
			return nil
		})
		m.outMu.Lock()
		meta.acked, meta.ackedAt = true, now
		m.outMu.Unlock()
	}
}

// handle runs the handler of one message, recovering from a panic, and marks the message handled.
func (m *Messenger) handle(ctx context.Context, msg *Message) {
	h := m.handler(msg.Type)
	if h == nil {
		m.log.Debug("no handler", "type", msg.Type)
		m.markHandled(msg)
		return
	}
	hctx, cancel := context.WithTimeout(ctx, m.handlerTimeout)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		completed := false
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("handler panicked", "type", msg.Type, "order", msg.OrderID, "from", short(msg.From), "panic", r, "stack", string(debug.Stack()))
				completed = true // running it again would panic again
			}
			done <- completed
		}()
		h(hctx, msg)
		completed = true
	}()
	select {
	case completed := <-done:
		if completed {
			m.markHandled(msg)
		}
	case <-hctx.Done():
		if ctx.Err() != nil {
			return // shutting down: handled again at the next start
		}
		// the handler ignores its context; free the worker, the message is handled again at the next start
		m.log.Error("handler timed out", "type", msg.Type, "order", msg.OrderID, "after", m.handlerTimeout)
	}
}

func (m *Messenger) markHandled(msg *Message) {
	err := store.Modify(m.db, bucketInbox, msg.Inner.ID, func(v *inboxEntry, exists bool) error {
		if !exists {
			return store.ErrStop
		}
		v.Pending, v.Handled = false, time.Now().Unix()
		return nil
	})
	if err != nil {
		m.log.Warn("mark handled", "id", msg.Inner.ID, "err", err)
	}
}

// redispatch queues the messages a previous run stored but did not finish handling, oldest first.
func (m *Messenger) redispatch() {
	var pending []inboxEntry
	_ = m.db.ForEach(bucketInbox, func(_ string, raw []byte) error {
		var e inboxEntry
		if json.Unmarshal(raw, &e) == nil && e.Pending && e.Inner != nil {
			pending = append(pending, e)
		}
		return nil
	})
	slices.SortStableFunc(pending, func(a, b inboxEntry) int {
		if a.Inner.CreatedAt != b.Inner.CreatedAt {
			return int(a.Inner.CreatedAt - b.Inner.CreatedAt)
		}
		return int(a.Received - b.Received)
	})
	for _, e := range pending {
		msg := &Message{Inner: e.Inner, From: e.Inner.PubKey, Type: giftwrap.Type(e.Inner), OrderID: giftwrap.OrderID(e.Inner), Relay: e.Relay}
		m.log.Info("handling a message left from the last run", "type", msg.Type, "order", msg.OrderID)
		m.disp.enqueue(msg)
	}
}

func (m *Messenger) pruneLoop(ctx context.Context) {
	t := time.NewTicker(PruneInterval)
	defer t.Stop()
	for {
		m.prune(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func retention(orderID string) time.Duration {
	if orderID == "" {
		return OtherRetention
	}
	return HistoryRetention
}

// prune forgets old wrap ids, handled inbox entries and acked or expired outbox entries.
func (m *Messenger) prune(now time.Time) (n int) {
	var wraps, inbox, outbox []string
	_ = m.db.ForEach(bucketWraps, func(id string, raw []byte) error {
		var at int64
		if json.Unmarshal(raw, &at) != nil || now.Sub(time.Unix(at, 0)) > WrapMaxAge {
			wraps = append(wraps, id)
		}
		return nil
	})
	_ = m.db.ForEach(bucketInbox, func(id string, raw []byte) error {
		var e inboxEntry
		if json.Unmarshal(raw, &e) != nil || e.Inner == nil {
			inbox = append(inbox, id)
		} else if !e.Pending && now.Sub(time.Unix(e.Received, 0)) > retention(giftwrap.OrderID(e.Inner)) {
			inbox = append(inbox, id)
		}
		return nil
	})
	m.outMu.Lock()
	for id, meta := range m.outbox {
		if (meta.acked || m.expired(meta, now)) && now.Sub(time.Unix(meta.created, 0)) > retention(meta.orderID) {
			outbox = append(outbox, id)
		}
	}
	m.outMu.Unlock()
	for _, id := range wraps {
		_ = m.db.Delete(bucketWraps, id)
	}
	for _, id := range inbox {
		_ = m.db.Delete(bucketInbox, id)
	}
	for _, id := range outbox {
		if m.db.Delete(bucketOutbox, id) == nil {
			m.outMu.Lock()
			delete(m.outbox, id)
			m.outMu.Unlock()
		}
	}
	if n = len(wraps) + len(inbox) + len(outbox); n > 0 {
		m.log.Info("pruned", "wraps", len(wraps), "inbox", len(inbox), "outbox", len(outbox))
	}
	return n
}

// Inbox returns every stored received inner event of an order (all orders if orderID is empty).
func (m *Messenger) Inbox(orderID string) []*nostr.Event {
	var out []*nostr.Event
	_ = m.db.ForEach(bucketInbox, func(_ string, raw []byte) error {
		var e inboxEntry
		if json.Unmarshal(raw, &e) == nil && e.Inner != nil && (orderID == "" || giftwrap.OrderID(e.Inner) == orderID) && giftwrap.Type(e.Inner) != proto.TypeAck {
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
		if json.Unmarshal(raw, &e) == nil && e.Inner != nil && (orderID == "" || giftwrap.OrderID(e.Inner) == orderID) && giftwrap.Type(e.Inner) != proto.TypeAck {
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
