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
	"sync/atomic"
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
	// StrangerRate and StrangerBurst limit the messages accepted per minute from all senders together that are
	// not a counterparty of ours (§4.10); counterparties (a party of an order we keep) are not limited.
	StrangerRate  = 60
	StrangerBurst = 60
	// CounterpartyRate and CounterpartyBurst are only a backstop per (counterparty, order), far above what an
	// order needs (a dispute's attachments come in a burst of up to 4 × 64), against a party flooding its order.
	CounterpartyRate  = 300
	CounterpartyBurst = 600

	// BacklogPage and BacklogInterval: stored wraps are read page by page backwards (until) down to WrapMaxAge at
	// start and then every BacklogInterval, so that more than one subscription limit of junk cannot hide older
	// messages (§4.10: the subscription itself reads the newest 1000).
	SubscribeLimit  = 1000
	BacklogPage     = 500
	BacklogInterval = time.Hour
	// AckWorkers send the queued acks; acks are queued per recipient and never dropped.
	AckWorkers = 4

	// WrapMaxAge: older wraps are ignored and their ids forgotten (a resend stops after RetryLimit anyway).
	WrapMaxAge = 14 * 24 * time.Hour
	// HistoryRetention keeps handled inbox and finished outbox entries of an order (disputes read them as
	// evidence until T2: t1 = (delivery_days + 21) days and t2 = t1 + 14 days by default, and a user may accept
	// up to max_t2 = 120 days, §4.5.1); OtherRetention is for messages without an order.
	HistoryRetention = 120 * 24 * time.Hour
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

// RetryFunc tells whether a pubkey is a counterparty of an order we keep: unacknowledged messages to it about the
// order are resent (§4.10: replies to somebody without an order with us are sent once), and messages from it
// about the order are not rate limited.
type RetryFunc func(to, orderID string) bool

// AcceptFunc tells whether a message from a sender that is not a counterparty is taken at all (§4.10: a message no
// role accepts is neither stored nor acked). Counterparties are always accepted.
type AcceptFunc func(msg *Message) bool

// HintsFunc returns relays where the sender of a message reads (e.g. the relays of its order request), used for
// the ack and other replies when the sender has no inbox relays (10050).
type HintsFunc func(msg *Message) []string

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
	accepts     AcceptFunc
	hints       HintsFunc
	log         *slog.Logger

	mu       sync.Mutex
	handlers map[string]Handler
	fallback Handler

	outMu  sync.Mutex
	outbox map[string]*outMeta

	// the package defaults at New
	retryInterval, retryLimit, handlerTimeout time.Duration
	subLimit, backlogPage                     int
	backlogInterval                           time.Duration

	disp      *dispatcher
	senders   *rateLimiter
	strangers *rateLimiter // all non-counterparty senders together (one key)
	parties   *rateLimiter // per (counterparty, order)

	// known remembers (sender, order) of messages stored in this run, so that the next message of a just
	// accepted request counts as a counterparty's before the handler created the order
	knownMu sync.Mutex
	known   map[string]time.Time

	ackMu    sync.Mutex
	ackq     map[ackKey]*ackBatch
	ackOrder []ackKey
	ackWake  chan struct{}
	acked    map[string]time.Time // inner id → when an ack was last queued (duplicates are not acked every time)

	paused atomic.Bool
}

type ackKey struct{ to, orderID string }

type ackBatch struct {
	ids   []string
	hints []string
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
	Retry    RetryFunc  // nil: resend everything, nobody is a known counterparty
	Accepts  AcceptFunc // nil: accept every message
	Hints    HintsFunc  // nil: no hints but the relay a message came from
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
		resolve: c.Resolve, retry: c.Retry, accepts: c.Accepts, hints: c.Hints, log: c.Log.With("component", "messenger"),
		handlers: map[string]Handler{}, outbox: map[string]*outMeta{},
		senders: newRateLimiter(SenderRate, SenderBurst), strangers: newRateLimiter(StrangerRate, StrangerBurst),
		parties: newRateLimiter(CounterpartyRate, CounterpartyBurst),
		known:   map[string]time.Time{},
		ackq:    map[ackKey]*ackBatch{}, ackWake: make(chan struct{}, 1), acked: map[string]time.Time{},
		retryInterval: RetryInterval, retryLimit: RetryLimit, handlerTimeout: HandlerTimeout,
		subLimit: SubscribeLimit, backlogPage: max(BacklogPage, 1), backlogInterval: BacklogInterval,
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
// ctx ends. Register the handlers before. Start may be called again after the context of the previous call ended
// (Pause / resume of the node); handlers still running from before keep their order busy until they return.
func (m *Messenger) Start(ctx context.Context) {
	m.disp.start(ctx)
	m.redispatch()
	filter := nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {m.Pub}}, Limit: m.subLimit}
	m.pool.Subscribe(ctx, m.inbox, nostr.Filters{filter}, func(relay string, ev *nostr.Event) {
		m.receive(ctx, relay, ev)
	})
	for i := 0; i < max(AckWorkers, 1); i++ {
		go m.ackLoop(ctx)
	}
	go m.backlogLoop(ctx)
	go m.resendLoop(ctx)
	go m.pruneLoop(ctx)
}

// SetPaused stops (true) or resumes publishing: while paused nothing is sent, messages to send are kept in the
// outbox and go out with the resend loop after the pause. The node pauses the whole network activity with it and
// by ending the context of Start (admin POST /admin/pause).
func (m *Messenger) SetPaused(p bool) { m.paused.Store(p) }

// Paused tells whether publishing is paused.
func (m *Messenger) Paused() bool { return m.paused.Load() }

// backlogLoop reads the stored wraps of our inbox relays backwards, page by page, down to WrapMaxAge.
func (m *Messenger) backlogLoop(ctx context.Context) {
	t := time.NewTicker(m.backlogInterval)
	defer t.Stop()
	for {
		for _, relay := range m.inbox {
			m.readBacklog(ctx, relay)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Messenger) readBacklog(ctx context.Context, relay string) {
	since := nostr.Timestamp(time.Now().Add(-WrapMaxAge).Unix())
	until := nostr.Now()
	for ctx.Err() == nil {
		f := nostr.Filter{Kinds: []int{giftwrap.KindWrap}, Tags: nostr.TagMap{"p": {m.Pub}}, Since: &since, Until: &until, Limit: m.backlogPage}
		evs := m.pool.Query(ctx, []string{relay}, f)
		oldest := until
		for _, ev := range evs {
			m.receive(ctx, relay, ev)
			oldest = min(oldest, ev.CreatedAt)
		}
		if len(evs) < m.backlogPage {
			return
		}
		if oldest >= until { // a full page within one second: step past it
			oldest = until - 1
		}
		if oldest < since {
			return
		}
		until = oldest
	}
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
	return m.capRelays(r)
}

// capRelays drops duplicates and the relays the pool would refuse (private addresses unless allowed, bad URLs)
// before it takes the first MaxRelays, so that unusable entries do not take the places (§4.10).
func (m *Messenger) capRelays(r []string) []string {
	out := make([]string, 0, min(len(r), MaxRelays))
	for _, u := range r {
		if len(out) == MaxRelays {
			break
		}
		if u != "" && !slices.Contains(out, u) && (m.pool == nil || m.pool.Allowed(u)) {
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
	// relays refuse larger contents, so the message would never arrive (§4.9)
	if n := len(wrap.Content); n > giftwrap.MaxWrapContent {
		return nil, fmt.Errorf("%s message: wrap content is %d bytes, over the %d bytes relays store (§4.9; inner %d bytes)",
			typ, n, giftwrap.MaxWrapContent, len(inner.String()))
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
	if m.paused.Load() {
		// kept in the outbox: the resend loop sends it after the pause (acks are queued again by the duplicate)
		m.log.Debug("paused, not sending", "type", giftwrap.Type(e.Inner), "to", e.To)
		return
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
	// only an id that is the hash of this very wrap may be remembered as seen: a relay could otherwise send junk
	// under the id of a real wrap and have us drop the real one (the pool checks this too)
	if wrap.Kind != giftwrap.KindWrap || giftwrap.Verify(wrap) != nil {
		m.log.Debug("dropping an event with a wrong id or signature", "id", wrap.ID, "relay", relay)
		return
	}
	inner, err := giftwrap.Unwrap(m.secret, wrap)
	if err != nil {
		m.seen(wrap)
		m.log.Debug("dropping wrap", "id", wrap.ID, "relay", relay, "err", err)
		return
	}
	typ := giftwrap.Type(inner)
	msg := &Message{Inner: inner, From: inner.PubKey, Type: typ, OrderID: giftwrap.OrderID(inner), Relay: relay}
	if typ == proto.TypeAck {
		m.seen(wrap)
		m.markAcked(msg)
		return
	}
	// a message we already stored (a resend in a new wrap, or the copy on another relay) costs no rate limit
	// tokens: it is only acked again, because the sender resends until one of our acks arrives
	if m.db.Has(bucketInbox, inner.ID) {
		m.seen(wrap)
		m.queueAck(msg, false)
		return
	}
	if m.isCounterparty(msg.From, msg.OrderID) {
		if !m.parties.allow(msg.From+"|"+msg.OrderID, time.Now()) {
			m.log.Warn("counterparty flooding its order, dropping", "from", short(msg.From), "order", msg.OrderID, "type", typ)
			return
		}
	} else {
		// §4.10: what no role takes from a stranger is neither stored nor acked. The wrap is not remembered as seen,
		// so that it is looked at again at the next reconnect (the order may exist by then).
		if m.accepts != nil && !m.accepts(msg) {
			m.log.Debug("not accepted from a non-counterparty, dropping", "from", short(msg.From), "type", typ, "order", msg.OrderID)
			return
		}
		// not remembered as seen either: the sender's resend (or this wrap at our next reconnect) gets another chance
		now := time.Now()
		if !m.senders.allow(inner.PubKey, now) {
			m.log.Debug("sender over the rate limit, dropping", "from", short(inner.PubKey), "type", typ)
			return
		}
		if !m.strangers.allow("", now) {
			m.log.Debug("non-counterparty senders over the global rate limit, dropping", "from", short(inner.PubKey), "type", typ)
			return
		}
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
	m.seen(wrap)
	m.remember(msg.From, msg.OrderID)
	m.queueAck(msg, fresh)
	if !fresh {
		return
	}
	m.log.Info("received", "type", typ, "from", short(inner.PubKey), "order", msg.OrderID, "relay", relay)
	m.disp.enqueue(msg)
}

func (m *Messenger) seen(wrap *nostr.Event) { _ = m.db.Put(bucketWraps, wrap.ID, time.Now().Unix()) }

// knownTTL and maxKnown bound the memory of recently stored (sender, order) pairs.
const (
	knownTTL = 30 * time.Minute
	maxKnown = 10000
)

// isCounterparty tells whether from is a party of an order we keep, or sent a message about the order that we
// stored recently (its handler may not have created the order yet).
func (m *Messenger) isCounterparty(from, orderID string) bool {
	if orderID == "" {
		return false
	}
	m.knownMu.Lock()
	at, ok := m.known[from+"|"+orderID]
	m.knownMu.Unlock()
	if ok && time.Since(at) < knownTTL {
		return true
	}
	return m.retry != nil && m.retry(from, orderID)
}

func (m *Messenger) remember(from, orderID string) {
	if orderID == "" {
		return
	}
	now := time.Now()
	m.knownMu.Lock()
	defer m.knownMu.Unlock()
	if len(m.known) >= maxKnown {
		for k, at := range m.known {
			if now.Sub(at) >= knownTTL {
				delete(m.known, k)
			}
		}
		for k := range m.known { // still full: forget arbitrary ones
			if len(m.known) < maxKnown*9/10 {
				break
			}
			delete(m.known, k)
		}
	}
	m.known[from+"|"+orderID] = now
}

// ackRepeat: a duplicate is acked again only this long after the last ack of the same message, so that replaying
// a message does not make us send an ack per copy.
const (
	ackRepeat  = 10 * time.Second
	maxAckIDs  = 100 // ids per ack message
	maxAckSeen = 20000
)

// queueAck queues the ack of a stored message. Acks are batched per recipient and order and sent by the ack
// workers, so none is dropped under load. A duplicate (fresh false) is acked at most every ackRepeat.
func (m *Messenger) queueAck(msg *Message, fresh bool) {
	now := time.Now()
	id := msg.Inner.ID
	m.ackMu.Lock()
	last, ok := m.acked[id]
	m.ackMu.Unlock()
	if ok && !fresh && now.Sub(last) < ackRepeat {
		return
	}
	var hints []string
	if m.hints != nil {
		hints = m.hints(msg)
	}
	if msg.Relay != "" && !slices.Contains(hints, msg.Relay) {
		hints = append(hints, msg.Relay)
	}
	m.ackMu.Lock()
	defer m.ackMu.Unlock()
	if len(m.acked) >= maxAckSeen {
		for k, at := range m.acked {
			if now.Sub(at) >= ackRepeat {
				delete(m.acked, k)
			}
		}
	}
	m.acked[id] = now
	k := ackKey{to: msg.From, orderID: msg.OrderID}
	b := m.ackq[k]
	if b == nil {
		b = &ackBatch{}
		m.ackq[k] = b
		m.ackOrder = append(m.ackOrder, k)
	}
	if !slices.Contains(b.ids, id) {
		b.ids = append(b.ids, id)
	}
	for _, h := range hints {
		if !slices.Contains(b.hints, h) {
			b.hints = append(b.hints, h)
		}
	}
	select {
	case m.ackWake <- struct{}{}:
	default:
	}
}

// nextAck takes up to maxAckIDs queued ids of the first recipient.
func (m *Messenger) nextAck() (ackKey, []string, []string, bool) {
	m.ackMu.Lock()
	defer m.ackMu.Unlock()
	if len(m.ackOrder) == 0 {
		return ackKey{}, nil, nil, false
	}
	k := m.ackOrder[0]
	b := m.ackq[k]
	n := min(len(b.ids), maxAckIDs)
	ids := slices.Clone(b.ids[:n])
	hints := slices.Clone(b.hints)
	if n == len(b.ids) {
		delete(m.ackq, k)
		m.ackOrder = m.ackOrder[1:]
	} else {
		b.ids = b.ids[n:]
		m.ackOrder = append(m.ackOrder[1:], k)
	}
	if len(m.ackOrder) > 0 {
		select {
		case m.ackWake <- struct{}{}:
		default:
		}
	}
	return k, ids, hints, true
}

// ackLoop sends queued acks until ctx ends (what is left stays queued for the next Start).
func (m *Messenger) ackLoop(ctx context.Context) {
	for {
		if m.paused.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		k, ids, hints, ok := m.nextAck()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-m.ackWake:
				continue
			}
		}
		if _, err := m.Send(ctx, k.to, k.orderID, proto.TypeAck, proto.Ack{IDs: ids}, hints); err != nil {
			m.log.Warn("ack failed", "ids", len(ids), "to", short(k.to), "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// PendingAcks is the number of acks queued and not yet sent (for tests and status).
func (m *Messenger) PendingAcks() int {
	m.ackMu.Lock()
	defer m.ackMu.Unlock()
	n := 0
	for _, b := range m.ackq {
		n += len(b.ids)
	}
	return n
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

// handle runs the handler of one message, recovering from a panic, and marks the message handled when the handler
// returned within its time (and not because we shut down). When the handler overruns its timeout (or we shut
// down while it runs), handle returns a channel that closes when the handler finally returns: the dispatcher keeps
// the order busy until then (so that two handlers of one order never run at once) and frees the worker.
func (m *Messenger) handle(ctx context.Context, msg *Message) <-chan struct{} {
	h := m.handler(msg.Type)
	if h == nil {
		m.log.Debug("no handler", "type", msg.Type)
		m.markHandled(msg)
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, m.handlerTimeout)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("handler panicked", "type", msg.Type, "order", msg.OrderID, "from", short(msg.From), "panic", r, "stack", string(debug.Stack()))
				m.markHandled(msg) // running it again would panic again
			}
		}()
		h(hctx, msg)
		if hctx.Err() == nil {
			m.markHandled(msg)
		}
		// else it ran into its deadline or we are shutting down: handled again at the next start
	}()
	select {
	case <-finished:
		return nil
	case <-hctx.Done():
		select {
		case <-finished:
			return nil
		default:
		}
		if ctx.Err() == nil {
			m.log.Error("handler timed out; its order waits until it returns", "type", msg.Type, "order", msg.OrderID, "after", m.handlerTimeout)
		}
		return finished
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
