package trust

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

// A registry's events.json: the list comes before its delegation here, and there are events that must not count.
func TestBundleFeedsTheStoreLikeRelayEvents(t *testing.T) {
	c, op, sh, es, other := newActor(), newActor(), newActor(), newActor(), newActor()
	must := func(ev *nostr.Event, err error) *nostr.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	entry := Entry{Region: "JP-13", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}, Tags: []string{}, EscrowSLADays: 14}
	list := must(NewList(op.sk, 7, &List{Network: "ps-main", Name: "registry", Regions: []string{"JP-13"}, Entries: []Entry{entry}}))
	del := must(NewDelegation(c.sk, op.pk, "ps-main", 7, false, "registry operator"))
	foreignNet := must(NewDelegation(c.sk, other.pk, "ps-lab", 7, false, ""))
	outOfScope := must(NewDelegation(other.sk, op.pk, "ps-main", 7, false, "not our coordinator"))
	forged := must(NewDelegation(c.sk, other.pk, "ps-main", 8, false, ""))
	forged.Tags = forged.Tags.AppendUnique(nostr.Tag{"x", "tampered"}) // the id no longer matches

	body, _ := json.Marshal(map[string]any{"network": "ps-main", "version": 7, "events": []*nostr.Event{list, foreignNet, outOfScope, forged, del}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	evs, err := FetchBundle(context.Background(), srv.Client(), srv.URL+"/events.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("got %d events", len(evs))
	}
	s, _ := NewStore(nil)
	s.SetScope([]string{c.pk}, "ps-main")
	stored, rejected := s.PutBundle(evs, "ps-main")
	if len(stored) != 2 || rejected != 2 {
		t.Fatalf("stored %d, rejected %d; want the delegation and the list, and the forged and ps-lab events refused", len(stored), rejected)
	}
	rows := s.Effective([]string{c.pk}, "ps-main")
	if len(rows) != 1 || rows[0].Shopper != sh.pk || rows[0].Operator != op.pk || rows[0].ListVersion != 7 {
		t.Fatalf("effective set %+v", rows)
	}
	// the same bundle again is nothing new
	if again, _ := s.PutBundle(evs, "ps-main"); len(again) != 0 {
		t.Fatalf("stored %d again", len(again))
	}

	// a plain array works too; errors are reported
	arr, _ := json.Marshal([]*nostr.Event{del})
	if got, err := ParseBundle(arr); err != nil || len(got) != 1 {
		t.Fatalf("array bundle: %v %d", err, len(got))
	}
	if _, err := ParseBundle([]byte(`{"nope": 1}`)); err == nil {
		t.Fatal("a file without events was accepted")
	}
	if _, err := FetchBundle(context.Background(), srv.Client(), srv.URL+"/missing.json"); err == nil {
		t.Fatal("HTTP 404 was not an error")
	}
}
