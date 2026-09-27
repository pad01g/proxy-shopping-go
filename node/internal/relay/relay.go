// Package relay is the mailbox relay psrelay: a khatru Nostr relay with a badger event store that accepts only
// the kinds of the protocol, forgets gift wraps after the retention period, keeps only the latest version of the
// replaceable kinds (10050, 30500–30503) and rate limits its clients.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/fiatjaf/eventstore"
	eventbadger "github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	"github.com/nbd-wtf/go-nostr"
)

// DefaultKinds are the kinds a mailbox relay stores: deletions (of one's own events only), inbox relays, gift
// wraps and the trust / profile events. Seals (kind 13) only travel inside gift wraps and are refused, and the
// protocol has no use for metadata (kind 0).
var DefaultKinds = []int{5, 10050, 1059, 30500, 30501, 30502, 30503}

// Default limits. Every limit is per minute; a negative value in Options switches it off. Client addresses are
// IPv4 addresses and IPv6 /64 prefixes. The per-address limits are generous (many users can share one address
// behind a NAT); the per-connection limits do most of the work.
const (
	DefaultEventsPerMinute       = 3000 // per client address
	DefaultConnEventsPerMinute   = 300  // per connection
	DefaultConnMessagesPerMinute = 1200 // frames of any kind per connection, counted before parsing or verifying
	DefaultReqsPerMinute         = 3000 // filters of REQs per client address
	DefaultConnsPerMinute        = 600  // new connections per client address
	DefaultMaxSubscriptions      = 32   // open REQs per connection
	DefaultMaxLimit              = 1000
)

// Options configure a relay. Zero values take the defaults.
type Options struct {
	DataDir       string
	Kinds         []int
	RetentionDays int // for kind 1059
	MaxEventSize  int // bytes of the serialized event
	Name          string
	Log           *slog.Logger

	EventsPerMinute       int      // events per client address
	ConnEventsPerMinute   int      // events per connection
	ConnMessagesPerMinute int      // frames per connection (before parsing and signature checks)
	ReqsPerMinute         int      // REQ filters per client address
	ConnsPerMinute        int      // new connections per client address
	MaxSubscriptions      int      // open REQs per connection
	MaxLimit              int      // cap of a filter's limit
	TrustedProxies        []string // IPs / CIDRs of reverse proxies whose X-Forwarded-For names the client
}

// Server is a running relay.
type Server struct {
	opts  Options
	db    *eventbadger.BadgerBackend
	Relay *khatru.Relay
	log   *slog.Logger

	proxies    proxies
	ipEvents   *limiter[string]
	connEvents *limiter[*khatru.WebSocket]
	connFrames *limiter[*khatru.WebSocket]
	ipFilters  *limiter[string]
	connLimit  *limiter[string]
	subs       subscriptions

	replaceMu sync.Mutex // replacements are read-compare-write

	// khatru runs every client frame in a goroutine it does not track, so a store call may come after Close; the
	// store (badger) panics then. closeMu makes Close wait for running calls and refuse later ones.
	closeMu   sync.RWMutex
	closed    bool
	xffWarned atomic.Bool
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// New opens the store and configures the relay.
func New(opts Options) (*Server, error) {
	if len(opts.Kinds) == 0 {
		opts.Kinds = DefaultKinds
	}
	if opts.RetentionDays <= 0 {
		opts.RetentionDays = 30
	}
	if opts.MaxEventSize <= 0 {
		opts.MaxEventSize = 256 << 10
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	opts.EventsPerMinute = orDefault(opts.EventsPerMinute, DefaultEventsPerMinute)
	opts.ConnEventsPerMinute = orDefault(opts.ConnEventsPerMinute, DefaultConnEventsPerMinute)
	opts.ConnMessagesPerMinute = orDefault(opts.ConnMessagesPerMinute, DefaultConnMessagesPerMinute)
	opts.ReqsPerMinute = orDefault(opts.ReqsPerMinute, DefaultReqsPerMinute)
	opts.ConnsPerMinute = orDefault(opts.ConnsPerMinute, DefaultConnsPerMinute)
	opts.MaxSubscriptions = orDefault(opts.MaxSubscriptions, DefaultMaxSubscriptions)
	opts.MaxLimit = orDefault(opts.MaxLimit, DefaultMaxLimit)
	px, err := parseProxies(opts.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	db := &eventbadger.BadgerBackend{
		Path: opts.DataDir,
		BadgerOptionsModifier: func(o badger.Options) badger.Options {
			return o.WithLogger(nil).WithMemTableSize(16 << 20).WithNumVersionsToKeep(1)
		},
	}
	if err := db.Init(); err != nil {
		return nil, fmt.Errorf("open event store %s: %w", opts.DataDir, err)
	}
	s := &Server{
		opts: opts, db: db, Relay: khatru.NewRelay(), log: opts.Log.With("component", "psrelay"), proxies: px,
		ipEvents: newLimiter[string](opts.EventsPerMinute), connEvents: newLimiter[*khatru.WebSocket](opts.ConnEventsPerMinute),
		connFrames: newLimiter[*khatru.WebSocket](opts.ConnMessagesPerMinute),
		ipFilters:  newLimiter[string](opts.ReqsPerMinute), connLimit: newLimiter[string](opts.ConnsPerMinute),
		subs: subscriptions{open: map[*khatru.WebSocket]map[context.Context]struct{}{}},
	}
	r := s.Relay
	r.Info.Name = opts.Name
	r.Info.Description = "proxy-shopping mailbox relay"
	r.Info.SupportedNIPs = []any{1, 9, 11, 17, 40, 44, 59}
	r.MaxMessageSize = int64(opts.MaxEventSize) + 4096

	r.StoreEvent = append(r.StoreEvent, s.guarded(db.SaveEvent))
	r.QueryEvents = append(r.QueryEvents, s.query)
	r.DeleteEvent = append(r.DeleteEvent, s.guarded(db.DeleteEvent))
	// replaceable and addressable kinds (10050, 30500–30503) keep only their latest event, by v (§2.1)
	r.ReplaceEvent = append(r.ReplaceEvent, s.guarded(s.replaceEvent))
	r.RejectConnection = append(r.RejectConnection, s.rejectConnection)
	// a frame limit before khatru parses and verifies anything (it verifies signatures before RejectEvent)
	r.RejectMessage = append(r.RejectMessage, s.limitFrame)
	r.OnDisconnect = append(r.OnDisconnect, s.forgetConnection)
	// rate limits first, so that refused events cost no verification against the store
	r.RejectEvent = append(r.RejectEvent, s.limitEvent, s.rejectEvent, policies.PreventTimestampsInTheFuture(10*time.Minute))
	r.OverwriteFilter = append(r.OverwriteFilter, s.overwriteFilter)
	r.RejectFilter = append(r.RejectFilter, s.rejectFilter)
	r.OverwriteDeletionOutcome = append(r.OverwriteDeletionOutcome, s.deletionOutcome)
	// no COUNT (NIP-45): nothing in the protocol needs it, and it would bypass the REQ limits
	if len(px) == 0 {
		s.log.Info("no -trusted-proxies: X-Forwarded-For is ignored and limits apply to the peer address")
	}
	return s, nil
}

// versioned tells whether replacements of a kind are decided by the v tag (§2.1): the trust and profile kinds,
// and 10050 when both events carry a v (else created_at, as other clients expect).
func versioned(kind int) bool { return kind >= 30500 && kind <= 30503 }

// newer tells whether a replaces b (§2.1): the higher v, the same v and the smaller id. Kinds without versions (and
// 10050 unless both carry a v) compare created_at the same way.
func newer(a, b *nostr.Event) bool {
	va, vb := int64(a.CreatedAt), int64(b.CreatedAt)
	if versioned(a.Kind) || (a.Kind == 10050 && hasV(a) && hasV(b)) {
		va, vb = version(a), version(b)
	}
	if va != vb {
		return va > vb
	}
	return a.ID < b.ID
}

func hasV(ev *nostr.Event) bool { return ev.Tags.Find("v") != nil }

// version is the v tag, -1 when it is missing or malformed.
func version(ev *nostr.Event) int64 {
	if t := ev.Tags.Find("v"); len(t) >= 2 {
		if n, err := strconv.ParseInt(t[1], 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return -1
}

// replaceEvent stores a replaceable or addressable event unless a newer one of its address is stored, and deletes
// the older ones. Unlike the event store's own ReplaceEvent it decides by v, not created_at (§2.1): an old version
// signed again later (a newer created_at) must not evict a newer version.
func (s *Server) replaceEvent(ctx context.Context, ev *nostr.Event) error {
	s.replaceMu.Lock()
	defer s.replaceMu.Unlock()
	f := nostr.Filter{Kinds: []int{ev.Kind}, Authors: []string{ev.PubKey}, Limit: 100}
	if nostr.IsAddressableKind(ev.Kind) {
		f.Tags = nostr.TagMap{"d": []string{ev.Tags.GetD()}}
	}
	ch, err := s.db.QueryEvents(ctx, f)
	if err != nil {
		return fmt.Errorf("query the stored versions: %w", err)
	}
	var older []*nostr.Event
	for prev := range ch {
		if nostr.IsAddressableKind(ev.Kind) && prev.Tags.GetD() != ev.Tags.GetD() {
			continue
		}
		if prev.ID == ev.ID {
			return eventstore.ErrDupEvent
		}
		if newer(prev, ev) {
			return eventstore.ErrDupEvent // accepted as a duplicate: not stored and not passed to subscribers
		}
		older = append(older, prev)
	}
	if err := s.db.SaveEvent(ctx, ev); err != nil {
		return err
	}
	for _, prev := range older {
		if err := s.db.DeleteEvent(ctx, prev); err != nil {
			return fmt.Errorf("delete the older version %s: %w", prev.ID, err)
		}
	}
	return nil
}

func (s *Server) accepts(kind int) bool { return slices.Contains(s.opts.Kinds, kind) }

func (s *Server) rejectEvent(_ context.Context, ev *nostr.Event) (bool, string) {
	if !s.accepts(ev.Kind) {
		return true, fmt.Sprintf("blocked: kind %d is not accepted here", ev.Kind)
	}
	if n := len(ev.Serialize()); n > s.opts.MaxEventSize {
		return true, fmt.Sprintf("invalid: event of %d bytes exceeds %d", n, s.opts.MaxEventSize)
	}
	if ev.Kind == 1059 && ev.Tags.Find("p") == nil {
		return true, "invalid: gift wrap without recipient"
	}
	return false, ""
}

// ServeHTTP serves WebSocket and NIP-11 requests.
// GET /healthz answers 200 once the relay serves (for container health checks).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Header.Get("Upgrade") == "" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	s.Relay.ServeHTTP(w, r)
}

// Prune deletes gift wraps older than the retention period and returns how many.
func (s *Server) Prune(ctx context.Context) (int, error) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return 0, errClosed
	}
	until := nostr.Timestamp(time.Now().Add(-time.Duration(s.opts.RetentionDays) * 24 * time.Hour).Unix())
	n := 0
	for {
		ch, err := s.db.QueryEvents(ctx, nostr.Filter{Kinds: []int{1059}, Until: &until, Limit: 500})
		if err != nil {
			return n, fmt.Errorf("query expired wraps: %w", err)
		}
		var batch []*nostr.Event
		for ev := range ch {
			batch = append(batch, ev)
		}
		if len(batch) == 0 {
			return n, nil
		}
		for _, ev := range batch {
			if err := s.db.DeleteEvent(ctx, ev); err != nil {
				return n, fmt.Errorf("delete %s: %w", ev.ID, err)
			}
			n++
		}
	}
}

// RunPruner prunes on a ticker until ctx ends.
func (s *Server) RunPruner(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.Prune(ctx); err != nil {
			s.log.Warn("prune failed", "err", err)
		} else if n > 0 {
			s.log.Info("pruned expired gift wraps", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var errClosed = errors.New("error: relay is shutting down")

// guarded runs a store call unless the relay is closed.
func (s *Server) guarded(fn func(context.Context, *nostr.Event) error) func(context.Context, *nostr.Event) error {
	return func(ctx context.Context, ev *nostr.Event) error {
		s.closeMu.RLock()
		defer s.closeMu.RUnlock()
		if s.closed {
			return errClosed
		}
		return fn(ctx, ev)
	}
}

// query runs a store query unless the relay is closed; the results are collected before the lock is released.
func (s *Server) query(ctx context.Context, f nostr.Filter) (chan *nostr.Event, error) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return nil, errClosed
	}
	ch, err := s.db.QueryEvents(ctx, f)
	if err != nil {
		return nil, err
	}
	var evs []*nostr.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	out := make(chan *nostr.Event, len(evs))
	for _, ev := range evs {
		out <- ev
	}
	close(out)
	return out, nil
}

// Close closes the event store once running store calls returned.
func (s *Server) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if !s.closed {
		s.closed = true
		s.db.Close()
	}
}
