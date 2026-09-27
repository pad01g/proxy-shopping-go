package trust

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

const bucket = "trust"

// Store holds the newest version of every trust and profile event.
type Store struct {
	db *store.DB // may be nil (memory only)

	mu     sync.RWMutex
	events map[Key]*nostr.Event
}

// NewStore loads the persisted events.
func NewStore(db *store.DB) (*Store, error) {
	s := &Store{db: db, events: map[Key]*nostr.Event{}}
	if db == nil {
		return s, nil
	}
	evs, err := store.List[nostr.Event](db, bucket)
	if err != nil {
		return nil, fmt.Errorf("load trust store: %w", err)
	}
	for i := range evs {
		ev := evs[i]
		if Validate(&ev) == nil {
			s.events[KeyOf(&ev)] = &ev
		}
	}
	return s, nil
}

// Put validates and stores an event. It reports whether the event is new and newer than what we had.
func (s *Store) Put(ev *nostr.Event) (bool, error) {
	if err := Validate(ev); err != nil {
		return false, err
	}
	k := KeyOf(ev)
	s.mu.Lock()
	old, ok := s.events[k]
	if ok && (old.ID == ev.ID || !Newer(ev, old)) {
		s.mu.Unlock()
		return false, nil
	}
	s.events[k] = ev
	s.mu.Unlock()
	if s.db != nil {
		if err := s.db.Put(bucket, k.String(), ev); err != nil {
			return true, fmt.Errorf("persist %s: %w", k, err)
		}
	}
	return true, nil
}

// Get returns the newest event of a key.
func (s *Store) Get(k Key) *nostr.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.events[k]
}

// All returns every event, ordered by kind, pubkey and d.
func (s *Store) All() []*nostr.Event {
	s.mu.RLock()
	out := make([]*nostr.Event, 0, len(s.events))
	for _, ev := range s.events {
		out = append(out, ev)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := KeyOf(out[i]), KeyOf(out[j])
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.PubKey != b.PubKey {
			return a.PubKey < b.PubKey
		}
		return a.D < b.D
	})
	return out
}

// ByKind returns the events of one kind.
func (s *Store) ByKind(kind int) []*nostr.Event {
	var out []*nostr.Event
	for _, ev := range s.All() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// ShopperProfile returns the profile of a shopper in a network.
func (s *Store) ShopperProfile(pubkey, network string) (*ShopperProfile, *nostr.Event) {
	ev := s.Get(Key{Kind: KindShopperProfile, PubKey: pubkey, D: network})
	if ev == nil {
		return nil, nil
	}
	var p ShopperProfile
	if json.Unmarshal([]byte(ev.Content), &p) != nil {
		return nil, nil
	}
	return &p, ev
}

// EscrowProfile returns the profile of an escrow in a network.
func (s *Store) EscrowProfile(pubkey, network string) (*EscrowProfile, *nostr.Event) {
	ev := s.Get(Key{Kind: KindEscrowProfile, PubKey: pubkey, D: network})
	if ev == nil {
		return nil, nil
	}
	var p EscrowProfile
	if json.Unmarshal([]byte(ev.Content), &p) != nil {
		return nil, nil
	}
	return &p, ev
}

// InboxRelaysOf returns the 10050 relays of a pubkey.
func (s *Store) InboxRelaysOf(pubkey string) []string {
	if ev := s.Get(Key{Kind: KindInboxRelays, PubKey: pubkey}); ev != nil {
		return InboxRelays(ev)
	}
	return nil
}

// Row is one combination of the effective set with its origin (§2.4).
type Row struct {
	Entry
	Coordinator string  `json:"coordinator"`
	Operator    string  `json:"operator"`
	ListVersion int64   `json:"list_version"`
	ListID      string  `json:"list_id"`
	Relays      []Relay `json:"relays,omitempty"`
	ReportTo    string  `json:"report_to,omitempty"`
}

// Effective computes the effective combinations for the coordinators (highest priority first).
func (s *Store) Effective(coordinators []string, network string) []Row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type triple struct{ region, shopper, escrow string }
	seen := map[triple]bool{}
	var rows []Row
	for _, c := range coordinators {
		var operators []string
		for k, ev := range s.events {
			if k.Kind == KindDelegation && k.PubKey == c && Network(ev) == network && !Revoked(ev) {
				operators = append(operators, k.D)
			}
		}
		sort.Strings(operators)
		for _, op := range operators {
			ev := s.events[Key{Kind: KindList, PubKey: op, D: network}]
			if ev == nil {
				continue
			}
			l, err := ParseList(ev)
			if err != nil || (l.Network != "" && l.Network != network) {
				continue
			}
			for _, e := range l.Entries {
				t := triple{e.Region, e.Shopper, e.Escrow}
				if seen[t] {
					continue
				}
				seen[t] = true
				rows = append(rows, Row{Entry: e, Coordinator: c, Operator: op, ListVersion: Version(ev), ListID: ev.ID, Relays: l.Relays, ReportTo: l.ReportTo})
			}
		}
	}
	return rows
}

// Versions maps each operator with a list in the network to its list version (for /status).
func (s *Store) Versions(network string) map[string]int64 {
	out := map[string]int64{}
	for _, ev := range s.ByKind(KindList) {
		if Tag(ev, "d") == network {
			out[ev.PubKey] = Version(ev)
		}
	}
	return out
}

// Match describes what an order needs from the effective set.
type Match struct {
	Shopper, Escrow string
	Region          string // shop region
	Host            string // shop host
	Payment         string
}

// Find returns the effective rows allowing the order, the one naming the operator first.
func Find(rows []Row, m Match, preferOperator string) []Row {
	var out []Row
	for _, r := range rows {
		if r.Shopper != m.Shopper || r.Escrow != m.Escrow || !Covers(r.Region, m.Region) {
			continue
		}
		if m.Payment != "" && !slices.Contains(r.Payments, m.Payment) {
			continue
		}
		if m.Host != "" && !slices.Contains(r.Shops, "*") && !slices.Contains(r.Shops, m.Host) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Operator == preferOperator && out[j].Operator != preferOperator })
	return out
}
