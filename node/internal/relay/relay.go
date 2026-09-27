// Package relay is the mailbox relay psrelay: a khatru Nostr relay with a badger event store that accepts only
// the kinds of the protocol and forgets gift wraps after the retention period.
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

// DefaultKinds are the kinds a mailbox relay stores: metadata, deletions, inbox relays, gift wraps and the
// trust / profile events. Seals (kind 13) only travel inside gift wraps and are refused.
var DefaultKinds = []int{0, 5, 10050, 1059, 30500, 30501, 30502, 30503}

// Options configure a relay.
type Options struct {
	DataDir       string
	Kinds         []int
	RetentionDays int // for kind 1059
	MaxEventSize  int // bytes of the serialized event
	Name          string
	Log           *slog.Logger
}

// Server is a running relay.
type Server struct {
	opts  Options
	db    *eventbadger.BadgerBackend
	Relay *khatru.Relay
	log   *slog.Logger
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
	db := &eventbadger.BadgerBackend{
		Path: opts.DataDir,
		BadgerOptionsModifier: func(o badger.Options) badger.Options {
			return o.WithLogger(nil).WithMemTableSize(16 << 20).WithNumVersionsToKeep(1)
		},
	}
	if err := db.Init(); err != nil {
		return nil, fmt.Errorf("open event store %s: %w", opts.DataDir, err)
	}
	s := &Server{opts: opts, db: db, Relay: khatru.NewRelay(), log: opts.Log.With("component", "psrelay")}
	r := s.Relay
	r.Info.Name = opts.Name
	r.Info.Description = "proxy-shopping mailbox relay"
	r.Info.SupportedNIPs = []any{1, 9, 11, 17, 40, 44, 59}
	r.MaxMessageSize = int64(opts.MaxEventSize) + 4096

	r.StoreEvent = append(r.StoreEvent, db.SaveEvent)
	r.QueryEvents = append(r.QueryEvents, db.QueryEvents)
	r.CountEvents = append(r.CountEvents, db.CountEvents)
	r.DeleteEvent = append(r.DeleteEvent, db.DeleteEvent)
	r.ReplaceEvent = append(r.ReplaceEvent, db.ReplaceEvent)
	r.RejectEvent = append(r.RejectEvent, s.rejectEvent, policies.PreventTimestampsInTheFuture(10*time.Minute))
	return s, nil
}

func (s *Server) rejectEvent(_ context.Context, ev *nostr.Event) (bool, string) {
	if !slices.Contains(s.opts.Kinds, ev.Kind) {
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
