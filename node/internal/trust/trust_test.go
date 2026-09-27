package trust

import (
	"path/filepath"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

type actor struct{ sk, pk string }

func newActor() actor {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	return actor{sk, pk}
}

func mustPut(t *testing.T, s *Store, ev *nostr.Event, err error) bool {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.Put(ev)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		r, x string
		want bool
	}{
		{"JP", "JP-13-13104", true}, {"JP-13", "JP-13", true}, {"JP-13", "JP-13-13104", true},
		{"JP-1", "JP-13", false}, {"JP-13", "JP", false}, {"JP-27", "JP-13-13104", false},
	} {
		if got := Covers(c.r, c.x); got != c.want {
			t.Errorf("Covers(%q, %q) = %v", c.r, c.x, got)
		}
	}
}

func TestVersionRules(t *testing.T) {
	s, _ := NewStore(nil)
	c, op := newActor(), newActor()
	v2, err := NewDelegation(c.sk, op.pk, "ps-lab", 2, false, "")
	if !mustPut(t, s, v2, err) {
		t.Fatal("first version not accepted")
	}
	v1, err := NewDelegation(c.sk, op.pk, "ps-lab", 1, true, "")
	if mustPut(t, s, v1, err) {
		t.Fatal("older version replaced newer")
	}
	if mustPut(t, s, v2, nil) {
		t.Fatal("same event counted as new")
	}
	// same version: the smaller id wins
	a, _ := NewDelegation(c.sk, op.pk, "ps-lab", 3, false, "a")
	b, _ := NewDelegation(c.sk, op.pk, "ps-lab", 3, false, "b")
	lo, hi := a, b
	if hi.ID < lo.ID {
		lo, hi = hi, lo
	}
	mustPut(t, s, hi, nil)
	if !mustPut(t, s, lo, nil) {
		t.Fatal("smaller id of the same version not preferred")
	}
	if mustPut(t, s, hi, nil) {
		t.Fatal("larger id replaced the smaller one")
	}

	bad := *lo
	bad.Content = "tampered"
	if _, err := s.Put(&bad); err == nil {
		t.Fatal("tampered event accepted")
	}
}

func TestEffective(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, _ := NewStore(db)
	c1, c2, op1, op2, sh, es, es2 := newActor(), newActor(), newActor(), newActor(), newActor(), newActor(), newActor()

	put := func(ev *nostr.Event, err error) { mustPut(t, s, ev, err) }
	put(NewDelegation(c1.sk, op1.pk, "ps-lab", 1, false, ""))
	put(NewDelegation(c2.sk, op2.pk, "ps-lab", 1, false, ""))
	put(NewDelegation(c2.sk, op1.pk, "ps-lab", 1, false, ""))
	put(NewDelegation(c1.sk, op2.pk, "other-net", 1, false, ""))
	entry := func(region, shopper, escrow string) Entry {
		return Entry{Region: region, Shopper: shopper, Escrow: escrow, Shops: []string{"*"}, Payments: []string{"btc-signet"}}
	}
	put(NewList(op1.sk, 5, &List{Network: "ps-lab", Entries: []Entry{entry("JP-13", sh.pk, es.pk)}}))
	put(NewList(op2.sk, 2, &List{Network: "ps-lab", Entries: []Entry{entry("JP-13", sh.pk, es.pk), entry("JP", sh.pk, es2.pk)}}))

	rows := s.Effective([]string{c1.pk, c2.pk}, "ps-lab")
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Coordinator != c1.pk || rows[0].Operator != op1.pk || rows[0].ListVersion != 5 {
		t.Fatalf("first row has the wrong origin: %+v", rows[0])
	}
	if rows[1].Escrow != es2.pk || rows[1].Operator != op2.pk {
		t.Fatalf("second row %+v", rows[1])
	}
	if got := s.Effective(nil, "ps-lab"); len(got) != 0 {
		t.Fatal("no coordinators must trust nothing")
	}

	// revoking op2 under c2 removes its rows
	put(NewDelegation(c2.sk, op2.pk, "ps-lab", 2, true, "bad escrow"))
	rows = s.Effective([]string{c1.pk, c2.pk}, "ps-lab")
	if len(rows) != 1 || rows[0].Escrow != es.pk {
		t.Fatalf("after revocation %+v", rows)
	}

	m := Match{Shopper: sh.pk, Escrow: es.pk, Region: "JP-13-13104", Host: "safe-shop.test", Payment: "btc-signet"}
	if len(Find(rows, m, "")) != 1 {
		t.Fatal("matching row not found")
	}
	m.Payment = "usdc-evm"
	if len(Find(rows, m, "")) != 0 {
		t.Fatal("payment not checked")
	}
	m.Payment, m.Region = "btc-signet", "JP-27"
	if len(Find(rows, m, "")) != 0 {
		t.Fatal("region not checked")
	}

	// persisted
	s2, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.All()) != len(s.All()) {
		t.Fatal("store not persisted")
	}
	if v := s2.Versions("ps-lab"); v[op1.pk] != 5 {
		t.Fatalf("versions %v", v)
	}
}

func TestProfiles(t *testing.T) {
	s, _ := NewStore(nil)
	a := newActor()
	put := func(ev *nostr.Event, err error) { mustPut(t, s, ev, err) }
	put(NewProfile(a.sk, KindEscrowProfile, "ps-lab", 1, EscrowProfile{Name: "e", BTCXpub: "tpub"}))
	put(NewInboxRelays(a.sk, []string{"wss://r1", "wss://r2"}, 1))
	p, _ := s.EscrowProfile(a.pk, "ps-lab")
	if p == nil || p.Name != "e" {
		t.Fatal("profile")
	}
	if r := s.InboxRelaysOf(a.pk); len(r) != 2 {
		t.Fatalf("inbox %v", r)
	}
	if _, err := s.Put(&nostr.Event{Kind: 1}); err == nil {
		t.Fatal("kind 1 accepted")
	}
}
