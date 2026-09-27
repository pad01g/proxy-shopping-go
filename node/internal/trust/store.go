package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

const bucket = "trust"

// ErrOutOfScope is returned by Put for events of authors the configured coordinators do not reach (§10).
var ErrOutOfScope = errors.New("author is not reachable from the configured coordinators")

// Store holds the newest version of every trust and profile event.
type Store struct {
	db *store.DB // may be nil (memory only)

	mu     sync.RWMutex
	events map[Key]*nostr.Event
	scope  *scope // nil: everything valid is accepted (tests, tools)

	changed chan struct{}
}

// scope is the acceptance range of §10: the coordinators' delegations, the lists of the delegated operators, and
// the profiles and inbox relays of the shoppers and escrows of the effective set (plus our own).
type scope struct {
	coordinators []string
	network      string
	own          map[string]bool
	operators    map[string]bool
	participants map[string]bool
}

// NewStore loads the persisted events.
func NewStore(db *store.DB) (*Store, error) {
	s := &Store{db: db, events: map[Key]*nostr.Event{}, changed: make(chan struct{}, 1)}
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

// SetScope limits the store to what the coordinators reach (§10) in the network; own are pubkeys whose
// profiles and inbox relays are kept anyway (the node itself). Events already held outside the scope are
// dropped. It returns how many.
func (s *Store) SetScope(coordinators []string, network string, own ...string) int {
	s.mu.Lock()
	s.scope = &scope{coordinators: slices.Clone(coordinators), network: network, own: map[string]bool{}}
	for _, pk := range own {
		s.scope.own[pk] = true
	}
	dropped := s.rescopeLocked()
	s.mu.Unlock()
	s.persistDrops(dropped)
	s.signal()
	return len(dropped)
}

// Changes is signalled (coalesced) whenever the scope may have changed.
func (s *Store) Changes() <-chan struct{} { return s.changed }

func (s *Store) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// rescopeLocked recomputes operators and participants and drops the events outside the scope.
func (s *Store) rescopeLocked() []Key {
	sc := s.scope
	if sc == nil {
		return nil
	}
	sc.operators = map[string]bool{}
	for k, ev := range s.events {
		if k.Kind == KindDelegation && slices.Contains(sc.coordinators, k.PubKey) && Network(ev) == sc.network && !Revoked(ev) {
			sc.operators[k.D] = true
		}
	}
	sc.participants = map[string]bool{}
	for _, r := range s.effectiveLocked(sc.coordinators, sc.network) {
		sc.participants[r.Shopper] = true
		sc.participants[r.Escrow] = true
	}
	var dropped []Key
	for k, ev := range s.events {
		if !sc.allows(ev) {
			delete(s.events, k)
			dropped = append(dropped, k)
		}
	}
	return dropped
}

func (s *Store) persistDrops(keys []Key) {
	if s.db == nil {
		return
	}
	for _, k := range keys {
		_ = s.db.Delete(bucket, k.String())
	}
}

func (sc *scope) allows(ev *nostr.Event) bool {
	switch ev.Kind {
	case KindDelegation:
		return slices.Contains(sc.coordinators, ev.PubKey) && Network(ev) == sc.network
	case KindList:
		return sc.operators[ev.PubKey] && Tag(ev, "d") == sc.network
	case KindShopperProfile, KindEscrowProfile:
		return (sc.participants[ev.PubKey] || sc.own[ev.PubKey]) && Network(ev) == sc.network
	case KindInboxRelays:
		return sc.participants[ev.PubKey] || sc.own[ev.PubKey]
	}
	return false
}

// InScope tells whether the store would keep an event of this author (always true without a scope).
func (s *Store) InScope(ev *nostr.Event) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scope == nil || s.scope.allows(ev)
}

// Authors are the pubkeys of the scope, for subscriptions.
type Authors struct {
	Coordinators []string // their delegations
	Operators    []string // their lists
	Participants []string // their profiles and inbox relays (including our own)
}

// Authors returns the current scope; ok is false without one.
func (s *Store) Authors() (a Authors, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sc := s.scope
	if sc == nil {
		return a, false
	}
	a.Coordinators = slices.Sorted(slices.Values(sc.coordinators))
	for pk := range sc.operators {
		a.Operators = append(a.Operators, pk)
	}
	for pk := range sc.participants {
		a.Participants = append(a.Participants, pk)
	}
	for pk := range sc.own {
		if !sc.participants[pk] {
			a.Participants = append(a.Participants, pk)
		}
	}
	sort.Strings(a.Operators)
	sort.Strings(a.Participants)
	return a, true
}

// Put validates and stores an event. It reports whether the event is new and newer than what we had.
// With a scope, events of other authors are refused with ErrOutOfScope, and a new delegation or list re-evaluates
// the scope (dropping what fell out of it).
func (s *Store) Put(ev *nostr.Event) (bool, error) {
	if err := Validate(ev); err != nil {
		return false, err
	}
	k := KeyOf(ev)
	s.mu.Lock()
	if s.scope != nil && !s.scope.allows(ev) {
		s.mu.Unlock()
		return false, ErrOutOfScope
	}
	old, ok := s.events[k]
	if ok && (old.ID == ev.ID || !Newer(ev, old)) {
		s.mu.Unlock()
		return false, nil
	}
	s.events[k] = ev
	var dropped []Key
	rescoped := s.scope != nil && IsTrustKind(ev.Kind)
	if rescoped {
		dropped = s.rescopeLocked()
	}
	s.mu.Unlock()
	s.persistDrops(dropped)
	if rescoped {
		s.signal()
	}
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

// syncRank orders the kinds for trust-sync (§10): delegations, lists, then profiles and inbox relays, so that a
// receiver cutting the stream short still has the roots of trust.
func syncRank(kind int) int {
	switch kind {
	case KindDelegation:
		return 0
	case KindList:
		return 1
	case KindShopperProfile, KindEscrowProfile:
		return 2
	}
	return 3
}

// All returns every event: delegations, lists, profiles, inbox relays, each by pubkey and d.
func (s *Store) All() []*nostr.Event {
	s.mu.RLock()
	out := make([]*nostr.Event, 0, len(s.events))
	for _, ev := range s.events {
		out = append(out, ev)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := KeyOf(out[i]), KeyOf(out[j])
		if ra, rb := syncRank(a.Kind), syncRank(b.Kind); ra != rb {
			return ra < rb
		}
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
	return s.effectiveLocked(coordinators, network)
}

func (s *Store) effectiveLocked(coordinators []string, network string) []Row {
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
