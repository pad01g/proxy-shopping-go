// Package relay is the mailbox relay psrelay: a khatru Nostr relay with a badger event store that accepts only
// the kinds of the protocol, forgets gift wraps after the retention period, keeps only the latest version of the
// replaceable kinds (10050, 30500–30503) and rate limits its clients.
package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/dgraph-io/badger/v4"
	eventbadger "github.com/fiatjaf/eventstore/badger"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	"github.com/nbd-wtf/go-nostr"
)

// DefaultKinds are the kinds a mailbox relay stores: deletions (of one's own events only), inbox relays, gift
// wraps and the trust / profile events. Seals (kind 13) only travel inside gift wraps and are refused, and the
// protocol has no use for metadata (kind 0).
var DefaultKinds = []int{5, 10050, 1059, 30500, 30501, 30502, 30503}

// Default limits. Every limit is per minute; a negative value in Options switches it off.
const (
	DefaultEventsPerMinute     = 600 // per client address
	DefaultConnEventsPerMinute = 300 // per connection
	DefaultReqsPerMinute       = 600 // filters of REQs per client address
	DefaultConnsPerMinute      = 120 // new connections per client address
	DefaultMaxSubscriptions    = 32  // open REQs per connection
	DefaultMaxLimit            = 1000
)

// Options configure a relay. Zero values take the defaults.
type Options struct {
	DataDir       string
	Kinds         []int
	RetentionDays int // for kind 1059
	MaxEventSize  int // bytes of the serialized event
	Name          string
	Log           *slog.Logger

	EventsPerMinute     int      // events per client address
	ConnEventsPerMinute int      // events per connection
	ReqsPerMinute       int      // REQ filters per client address
	ConnsPerMinute      int      // new connections per client address
	MaxSubscriptions    int      // open REQs per connection
	MaxLimit            int      // cap of a filter's limit
	TrustedProxies      []string // IPs / CIDRs of reverse proxies whose X-Forwarded-For names the client
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
	ipFilters  *limiter[string]
	connLimit  *limiter[string]
	subs       subscriptions
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
		ipFilters: newLimiter[string](opts.ReqsPerMinute), connLimit: newLimiter[string](opts.ConnsPerMinute),
		subs: subscriptions{open: map[*khatru.WebSocket]map[context.Context]struct{}{}},
	}
	r := s.Relay
	r.Info.Name = opts.Name
	r.Info.Description = "proxy-shopping mailbox relay"
	r.Info.SupportedNIPs = []any{1, 9, 11, 17, 40, 44, 59}
	r.MaxMessageSize = int64(opts.MaxEventSize) + 4096

	r.StoreEvent = append(r.StoreEvent, db.SaveEvent)
	r.QueryEvents = append(r.QueryEvents, db.QueryEvents)
	r.DeleteEvent = append(r.DeleteEvent, db.DeleteEvent)
	// replaceable and addressable kinds (10050, 30500–30503) keep only their latest event
	r.ReplaceEvent = append(r.ReplaceEvent, db.ReplaceEvent)
	r.RejectConnection = append(r.RejectConnection, s.rejectConnection)
	// rate limits first, so that refused events cost no verification against the store
	r.RejectEvent = append(r.RejectEvent, s.limitEvent, s.rejectEvent, policies.PreventTimestampsInTheFuture(10*time.Minute))
	r.OverwriteFilter = append(r.OverwriteFilter, s.overwriteFilter)
	r.RejectFilter = append(r.RejectFilter, s.rejectFilter)
	r.OverwriteDeletionOutcome = append(r.OverwriteDeletionOutcome, s.deletionOutcome)
	// no COUNT (NIP-45): nothing in the protocol needs it, and it would bypass the REQ limits
	return s, nil
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
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.Relay.ServeHTTP(w, r) }

// Prune deletes gift wraps older than the retention period and returns how many.
func (s *Server) Prune(ctx context.Context) (int, error) {
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

// Close closes the event store.
func (s *Server) Close() { s.db.Close() }
