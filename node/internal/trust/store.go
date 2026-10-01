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

// ErrFull is returned by Put for a new address when the store holds its capacity (SetCapacity).
var ErrFull = errors.New("trust store is full")

// MaxParked is how many valid events outside the scope are kept aside, in case the scope grows to them: gossip
// and relays do not keep the order of delegation → list → profile, so a profile may arrive before the list that
// names its author (review 2, item 7).
var MaxParked = 1000

// Store holds the newest version of every trust and profile event.
type Store struct {
	db *store.DB // may be nil (memory only)

	mu       sync.RWMutex
	events   map[Key]*nostr.Event
	scope    *scope // nil: everything valid is accepted (tests, tools, the p2p relay)
	capacity int    // 0: unbounded
	maxPark  int    // MaxParked at NewStore
	parked   map[Key]*nostr.Event
	parkedQ  []Key // insertion order, for eviction

	watchMu  sync.Mutex
	watchers []chan struct{}
	changed  <-chan struct{}
	onAdmit  []func(ev *nostr.Event)
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
	s := &Store{db: db, events: map[Key]*nostr.Event{}, parked: map[Key]*nostr.Event{}, maxPark: max(MaxParked, 1)}
	s.changed = s.Watch()
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
	admitted, more := s.admitParkedLocked()
	dropped = append(dropped, more...)
	s.mu.Unlock()
	s.persistDrops(dropped)
	s.persistAdmitted(admitted)
	s.signal()
	return len(dropped)
}

// SetCapacity bounds the number of addresses held (0: unbounded). The p2p relay, which keeps every valid event
// without a scope, uses it.
func (s *Store) SetCapacity(n int) {
	s.mu.Lock()
	s.capacity = n
	s.mu.Unlock()
}

// Changes is signalled (coalesced) whenever the scope may have changed. It is the first watcher (the bridge).
func (s *Store) Changes() <-chan struct{} { return s.changed }

// Watch returns a new channel signalled (coalesced) whenever the scope may have changed.
func (s *Store) Watch() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.watchMu.Lock()
	s.watchers = append(s.watchers, ch)
	s.watchMu.Unlock()
	return ch
}

// OnAdmit registers a callback for parked events that the grown scope now admits (the store held them aside).
// It runs outside the store's lock.
func (s *Store) OnAdmit(fn func(ev *nostr.Event)) {
	s.watchMu.Lock()
	s.onAdmit = append(s.onAdmit, fn)
	s.watchMu.Unlock()
}

func (s *Store) signal() {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for _, ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Park keeps a valid event outside the scope aside (bounded by MaxParked; the newest version per address), so that
// it is admitted if the scope grows to its author. It reports whether it was kept.
func (s *Store) Park(ev *nostr.Event) bool {
	if Validate(ev) != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope == nil || s.scope.allows(ev) {
		return false
	}
	return s.parkLocked(ev)
}

func (s *Store) parkLocked(ev *nostr.Event) bool {
	k := KeyOf(ev)
	if old, ok := s.parked[k]; ok {
		if old.ID == ev.ID || !Newer(ev, old) {
			return false
		}
		s.parked[k] = ev
		return true
	}
	for len(s.parked) >= s.maxPark && len(s.parkedQ) > 0 {
		oldest := s.parkedQ[0]
		s.parkedQ = s.parkedQ[1:]
		delete(s.parked, oldest)
	}
	s.parked[k] = ev
	s.parkedQ = append(s.parkedQ, k)
	return true
}

// Parked is the number of events held aside.
func (s *Store) Parked() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.parked)
}

// admitParkedLocked moves the parked events the scope now allows into the store (again and again, as an admitted
// delegation or list widens the scope further) and returns them, and the keys a rescope dropped meanwhile.
func (s *Store) admitParkedLocked() (admitted []*nostr.Event, dropped []Key) {
	if s.scope == nil || len(s.parked) == 0 {
		return nil, nil
	}
	for {
		widened := false
		for k, ev := range s.parked {
			if !s.scope.allows(ev) {
				continue
			}
			delete(s.parked, k)
			if old, ok := s.events[k]; ok && (old.ID == ev.ID || !Newer(ev, old)) {
				continue
			}
			s.events[k] = ev
			admitted = append(admitted, ev)
			if IsTrustKind(ev.Kind) {
				widened = true
			}
		}
		if !widened {
			break
		}
		dropped = append(dropped, s.rescopeLocked()...)
	}
	q := s.parkedQ[:0]
	for _, k := range s.parkedQ {
		if _, ok := s.parked[k]; ok {
			q = append(q, k)
		}
	}
	s.parkedQ = q
	kept := admitted[:0]
	for _, ev := range admitted { // a later rescope may have dropped (and parked) an admitted one again
		if s.events[KeyOf(ev)] == ev {
			kept = append(kept, ev)
		}
	}
	return kept, dropped
}

func (s *Store) persistAdmitted(evs []*nostr.Event) {
	if len(evs) == 0 {
		return
	}
	if s.db != nil {
		for _, ev := range evs {
			_ = s.db.Put(bucket, KeyOf(ev).String(), ev)
		}
	}
	s.watchMu.Lock()
	fns := slices.Clone(s.onAdmit)
	s.watchMu.Unlock()
	for _, ev := range evs {
		for _, fn := range fns {
			fn(ev)
		}
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
			s.parkLocked(ev) // back if the scope grows to it again
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
		s.parkLocked(ev)
		s.mu.Unlock()
		return false, ErrOutOfScope
	}
	old, ok := s.events[k]
	if ok && (old.ID == ev.ID || !Newer(ev, old)) {
		s.mu.Unlock()
		return false, nil
	}
	if !ok && s.capacity > 0 && len(s.events) >= s.capacity {
		s.mu.Unlock()
		return false, ErrFull
	}
	s.events[k] = ev
	var dropped []Key
	var admitted []*nostr.Event
	rescoped := s.scope != nil && IsTrustKind(ev.Kind)
	if rescoped {
		dropped = s.rescopeLocked()
		var more []Key
		admitted, more = s.admitParkedLocked()
		dropped = append(dropped, more...)
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
	s.persistAdmitted(admitted)
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

// delegationsLocked returns the effective delegations of the coordinators in the network (not revoked), in the
// order of the coordinators and then of the operators.
func (s *Store) delegationsLocked(coordinators []string, network string) []*nostr.Event {
	var out []*nostr.Event
	for _, c := range coordinators {
		var dels []*nostr.Event
		for k, ev := range s.events {
			if k.Kind == KindDelegation && k.PubKey == c && Network(ev) == network && !Revoked(ev) {
				dels = append(dels, ev)
			}
		}
		sort.Slice(dels, func(i, j int) bool { return Tag(dels[i], "d") < Tag(dels[j], "d") })
		out = append(out, dels...)
	}
	return out
}

// ListURLs returns the list_url values (§2.2) of the effective delegations of the coordinators, without duplicates.
func (s *Store) ListURLs(coordinators []string, network string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, ev := range s.delegationsLocked(coordinators, network) {
		for _, u := range ListURLs(ev) {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	}
	return out
}

// MaxP2PRelays bounds the p2p relays taken from the effective lists together.
const MaxP2PRelays = 16

// P2PRelays returns the p2p_relays (§2.3) of the lists of the effectively delegated operators, without duplicates.
func (s *Store) P2PRelays(coordinators []string, network string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, del := range s.delegationsLocked(coordinators, network) {
		ev := s.events[Key{Kind: KindList, PubKey: Tag(del, "d"), D: network}]
		if ev == nil {
			continue
		}
		l, err := ParseList(ev)
		if err != nil {
			continue
		}
		for _, a := range l.P2PRelays {
			if len(out) == MaxP2PRelays {
				return out
			}
			if a != "" && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}
