package trust

import (
	"errors"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// Events arriving before the delegation or list that brings their author into the scope are parked and admitted
// when it arrives (review 2, item 7): a profile, then its list, then the delegation.
func TestParkedEventsAdmittedOnScopeChange(t *testing.T) {
	s, _ := NewStore(nil)
	c, op, sh, es := newActor(), newActor(), newActor(), newActor()
	s.SetScope([]string{c.pk}, "ps-lab")
	var admitted []int
	s.OnAdmit(func(ev *nostr.Event) { admitted = append(admitted, ev.Kind) })

	profile, _ := NewProfile(sh.sk, KindShopperProfile, "ps-lab", 1, ShopperProfile{Name: "s"})
	shIR, _ := NewInboxRelays(sh.sk, []string{"wss://r"}, 1)
	list, _ := NewList(op.sk, 1, &List{Network: "ps-lab", Entries: []Entry{{Region: "JP-13", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}}}})
	for _, ev := range []*nostr.Event{profile, shIR, list} {
		if _, err := s.Put(ev); !errors.Is(err, ErrOutOfScope) {
			t.Fatalf("kind %d before its delegation: %v", ev.Kind, err)
		}
	}
	if s.Parked() != 3 {
		t.Fatalf("%d parked, want 3", s.Parked())
	}
	d, _ := NewDelegation(c.sk, op.pk, "ps-lab", 1, false, "")
	if newer, err := s.Put(d); err != nil || !newer {
		t.Fatal(err)
	}
	for _, ev := range []*nostr.Event{profile, shIR, list} {
		if s.Get(KeyOf(ev)) == nil {
			t.Fatalf("kind %d not admitted after the delegation", ev.Kind)
		}
	}
	if len(admitted) != 3 || s.Parked() != 0 {
		t.Fatalf("admitted %v, %d still parked", admitted, s.Parked())
	}
	// revoking drops them again, parked; re-delegating brings them back without anyone sending them again
	rev, _ := NewDelegation(c.sk, op.pk, "ps-lab", 2, true, "")
	_, _ = s.Put(rev)
	if s.Get(KeyOf(list)) != nil || s.Parked() != 3 {
		t.Fatalf("revoked list kept (parked %d)", s.Parked())
	}
	again, _ := NewDelegation(c.sk, op.pk, "ps-lab", 3, false, "")
	_, _ = s.Put(again)
	if s.Get(KeyOf(list)) == nil || s.Get(KeyOf(profile)) == nil {
		t.Fatal("parked events not admitted after the new delegation")
	}
}

func TestParkBounded(t *testing.T) {
	old := MaxParked
	MaxParked = 5
	defer func() { MaxParked = old }()
	s, _ := NewStore(nil)
	s.SetScope([]string{newActor().pk}, "ps-lab")
	for i := 0; i < 20; i++ {
		ev, _ := NewInboxRelays(newActor().sk, []string{"wss://r"}, 1)
		s.Park(ev)
	}
	if s.Parked() != 5 {
		t.Fatalf("%d parked, cap 5", s.Parked())
	}
}

// A store with a capacity refuses new addresses when full but still takes newer versions (item 6).
func TestCapacity(t *testing.T) {
	s, _ := NewStore(nil)
	s.SetCapacity(2)
	a, b, c := newActor(), newActor(), newActor()
	for _, x := range []actor{a, b} {
		ev, _ := NewInboxRelays(x.sk, []string{"wss://r"}, 1)
		mustPut(t, s, ev, nil)
	}
	ev, _ := NewInboxRelays(c.sk, []string{"wss://r"}, 1)
	if _, err := s.Put(ev); !errors.Is(err, ErrFull) {
		t.Fatalf("third address: %v", err)
	}
	newer, _ := NewInboxRelays(a.sk, []string{"wss://r2"}, 2)
	if ok, err := s.Put(newer); err != nil || !ok {
		t.Fatalf("newer version when full: %v", err)
	}
}

// A list must name its network the same in d, the network tag and the content (item 9).
func TestListNetworkConsistent(t *testing.T) {
	op, sh, es := newActor(), newActor(), newActor()
	content := `{"network":"ps-main","entries":[{"region":"JP","shopper":"` + sh.pk + `","escrow":"` + es.pk + `"}]}`
	for name, tags := range map[string]nostr.Tags{
		"content differs": {{"d", "ps-lab"}, {"v", "1"}, {"network", "ps-lab"}},
		"tag differs":     {{"d", "ps-main"}, {"v", "1"}, {"network", "ps-lab"}},
		"no network tag":  {{"d", "ps-main"}, {"v", "1"}},
	} {
		ev := &nostr.Event{Kind: KindList, CreatedAt: nostr.Now(), Tags: tags, Content: content}
		_ = ev.Sign(op.sk)
		if Validate(ev) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := &nostr.Event{Kind: KindList, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "ps-main"}, {"v", "1"}, {"network", "ps-main"}}, Content: content}
	_ = ok.Sign(op.sk)
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
}

// 10050 is signed without v (its version is created_at), and events with a v are accepted too (item 9).
func TestInboxRelaysVersion(t *testing.T) {
	a := newActor()
	ev, _ := NewInboxRelays(a.sk, []string{"wss://r"}, 1790000000)
	if Tag(ev, "v") != "" || Version(ev) != 1790000000 || int64(ev.CreatedAt) != 1790000000 || Validate(ev) != nil {
		t.Fatalf("10050: %v", ev)
	}
	withV := &nostr.Event{Kind: KindInboxRelays, CreatedAt: 5, Tags: nostr.Tags{{"v", "1790000001"}, {"relay", "wss://r2"}}}
	_ = withV.Sign(a.sk)
	if Validate(withV) != nil || !Newer(withV, ev) {
		t.Fatal("10050 with a v tag")
	}
}
