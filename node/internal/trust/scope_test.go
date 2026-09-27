package trust

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

// §10: only events of authors reachable from the coordinators are accepted, and a trust change re-evaluates.
func TestScope(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, _ := NewStore(db)
	c, stranger, op, op2, sh, es, self, user := newActor(), newActor(), newActor(), newActor(), newActor(), newActor(), newActor(), newActor()

	// held from before the scope was set: dropped
	junk, _ := NewInboxRelays(user.sk, []string{"wss://r"}, 1)
	mustPut(t, s, junk, nil)
	if n := s.SetScope([]string{c.pk}, "ps-lab", self.pk); n != 1 || s.Get(KeyOf(junk)) != nil || db.Has(bucket, KeyOf(junk).String()) {
		t.Fatalf("SetScope dropped %d", n)
	}
	<-s.Changes()

	refused := func(what string, ev *nostr.Event, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(ev); !errors.Is(err, ErrOutOfScope) {
			t.Fatalf("%s: %v", what, err)
		}
	}
	d, err := NewDelegation(stranger.sk, op.pk, "ps-lab", 1, false, "")
	refused("delegation of another coordinator", d, err)
	d, err = NewDelegation(c.sk, op.pk, "other-net", 1, false, "")
	refused("delegation of another network", d, err)
	entry := Entry{Region: "JP-13", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}}
	l, err := NewList(op.sk, 1, &List{Network: "ps-lab", Entries: []Entry{entry}})
	refused("list before its delegation", l, err)
	p, err := NewProfile(sh.sk, KindShopperProfile, "ps-lab", 1, ShopperProfile{Name: "s"})
	refused("profile of an unlisted shopper", p, err)
	ir, err := NewInboxRelays(user.sk, []string{"wss://r"}, 1)
	refused("10050 of a user", ir, err)
	own, err := NewInboxRelays(self.sk, []string{"wss://r"}, 1)
	mustPut(t, s, own, err)

	deleg, err := NewDelegation(c.sk, op.pk, "ps-lab", 1, false, "")
	mustPut(t, s, deleg, err)
	<-s.Changes()
	l2, err := NewList(op2.sk, 1, &List{Network: "ps-lab", Entries: []Entry{entry}})
	refused("list of an operator without delegation", l2, err)
	mustPut(t, s, l, nil)
	mustPut(t, s, p, nil)
	esIR, err := NewInboxRelays(es.sk, []string{"wss://r"}, 1)
	mustPut(t, s, esIR, err)
	p2, err := NewProfile(sh.sk, KindShopperProfile, "other-net", 1, ShopperProfile{Name: "s"})
	refused("profile of another network", p2, err)
	a, _ := s.Authors()
	if len(a.Coordinators) != 1 || len(a.Operators) != 1 || a.Operators[0] != op.pk || len(a.Participants) != 3 {
		t.Fatalf("authors %+v", a)
	}

	// trust-sync order: delegations, lists, then profiles and 10050
	all := s.All()
	if all[0].Kind != KindDelegation || all[1].Kind != KindList || all[2].Kind != KindShopperProfile || all[len(all)-1].Kind != KindInboxRelays {
		var kinds []int
		for _, ev := range all {
			kinds = append(kinds, ev.Kind)
		}
		t.Fatalf("order %v", kinds)
	}

	// revoking the operator drops its list and the profiles of its shoppers and escrows, but not our own
	rev, err := NewDelegation(c.sk, op.pk, "ps-lab", 2, true, "")
	mustPut(t, s, rev, err)
	for _, ev := range []*nostr.Event{l, p, esIR} {
		if s.Get(KeyOf(ev)) != nil || db.Has(bucket, KeyOf(ev).String()) {
			t.Fatalf("kind %d kept after the revocation", ev.Kind)
		}
	}
	if s.Get(KeyOf(own)) == nil || s.Get(KeyOf(rev)) == nil {
		t.Fatal("own events or the revocation dropped")
	}
	if !s.InScope(own) || s.InScope(l) {
		t.Fatal("InScope")
	}
	// without a scope (tests, tools) everything valid is accepted
	open, _ := NewStore(nil)
	mustPut(t, open, ir, nil)
}
