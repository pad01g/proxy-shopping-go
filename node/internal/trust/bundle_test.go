package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// §2.6: https only, redirects within the origin only, 2 MiB and 1000 events at most, each event on its own.
func TestFetchBundleLimits(t *testing.T) {
	c, op := newActor(), newActor()
	del, err := NewDelegationWithURLs(c.sk, op.pk, "ps-main", 1, false, "", []string{"https://op.example/b.json"})
	if err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(del)
	forgedEv := *del
	forgedEv.Content = `{"note":"forged"}`
	forged, _ := json.Marshal(&forgedEv)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"events":[` + string(good) + `]}`))
	}))
	defer other.Close()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			// a malformed event and a forged one do not cost the good one
			_, _ = w.Write([]byte(`{"events":[{"id":5},` + string(forged) + `,` + string(good) + `,{"kind":"x"}]}`))
		case "/same":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, other.URL+"/ok", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(`{"events":[`))
			_, _ = w.Write(bytes.Repeat([]byte(" "), int(MaxBundleBytes)))
			_, _ = w.Write([]byte(`]}`))
		case "/many":
			evs := make([]json.RawMessage, MaxBundleEvents+5)
			for i := range evs {
				evs[i] = good
			}
			data, _ := json.Marshal(map[string]any{"events": evs})
			_, _ = w.Write(data)
		}
	}))
	defer srv.Close()
	client := srv.Client()
	// trust the other server's certificate too
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(other.Certificate())
	ctx := context.Background()

	evs, err := FetchBundle(ctx, client, srv.URL+"/ok")
	if err != nil || len(evs) != 2 {
		t.Fatalf("ok: %v, %d events (want the good one and the one that fails Validate later)", err, len(evs))
	}
	s, _ := NewStore(nil)
	if stored, rejected := s.PutBundle(evs, "ps-main"); len(stored) != 1 || rejected != 1 {
		t.Fatalf("stored %d, rejected %d", len(stored), rejected)
	}
	if _, err := FetchBundle(ctx, client, srv.URL+"/same"); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if _, err := FetchBundle(ctx, client, srv.URL+"/cross"); err == nil {
		t.Fatal("redirect to another origin followed")
	}
	if _, err := FetchBundle(ctx, client, other.URL+"/ok"); err != nil {
		t.Fatalf("the other origin itself: %v", err)
	}
	if _, err := FetchBundle(ctx, client, srv.URL+"/big"); err == nil {
		t.Fatal("bundle over 2 MiB accepted")
	}
	if evs, err := FetchBundle(ctx, client, srv.URL+"/many"); err != nil || len(evs) != MaxBundleEvents {
		t.Fatalf("many: %v, %d events", err, len(evs))
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(good) }))
	defer plain.Close()
	if _, err := FetchBundle(ctx, plain.Client(), plain.URL+"/ok"); err == nil {
		t.Fatal("http bundle fetched")
	}
}

// list_url (§2.2) and p2p_relays (§2.3) of the effective delegations and lists only.
func TestListURLsAndP2PRelays(t *testing.T) {
	c, other, op1, op2, op3, sh, es := newActor(), newActor(), newActor(), newActor(), newActor(), newActor(), newActor()
	must := func(ev *nostr.Event, err error) *nostr.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	s, _ := NewStore(nil)
	s.SetScope([]string{c.pk}, "ps-main")
	d1 := must(NewDelegationWithURLs(c.sk, op1.pk, "ps-main", 1, false, "", []string{"https://op1.example/b.json"}))
	// a non-https list_url (signed in by hand) is ignored, the delegation counts
	d2 := must(NewDelegation(c.sk, op2.pk, "ps-main", 1, false, ""))
	d2.Tags = append(d2.Tags, nostr.Tag{"list_url", "http://op2.example/b.json"}, nostr.Tag{"list_url", "https://op2.example/b.json"})
	if err := d2.Sign(c.sk); err != nil {
		t.Fatal(err)
	}
	revoked := must(NewDelegationWithURLs(c.sk, op3.pk, "ps-main", 1, true, "", []string{"https://op3.example/b.json"}))
	foreign := must(NewDelegationWithURLs(other.sk, op3.pk, "ps-main", 1, false, "", []string{"https://evil.example/b.json"}))
	entry := Entry{Region: "JP", Shopper: sh.pk, Escrow: es.pk, Shops: []string{"*"}, Payments: []string{"btc-signet"}, Tags: []string{}, EscrowSLADays: 14}
	l1 := must(NewList(op1.sk, 1, &List{Network: "ps-main", Name: "1", Entries: []Entry{entry}, P2PRelays: []string{"/dns4/r1.example/tcp/443/tls/ws/p2p/A", "/dns4/r1.example/tcp/443/tls/ws/p2p/A"}}))
	l3 := must(NewList(op3.sk, 1, &List{Network: "ps-main", Name: "3", Entries: []Entry{entry}, P2PRelays: []string{"/dns4/r3.example/tcp/443/tls/ws/p2p/B"}}))
	for _, ev := range []*nostr.Event{d1, d2, revoked, foreign, l1, l3} {
		_, _ = s.Put(ev)
	}
	urls := s.ListURLs([]string{c.pk}, "ps-main")
	if len(urls) != 2 || !slices.Contains(urls, "https://op1.example/b.json") || !slices.Contains(urls, "https://op2.example/b.json") {
		t.Fatalf("list urls %v", urls)
	}
	if relays := s.P2PRelays([]string{c.pk}, "ps-main"); len(relays) != 1 || relays[0] != "/dns4/r1.example/tcp/443/tls/ws/p2p/A" {
		t.Fatalf("p2p relays %v (the revoked operator's list must not count)", relays)
	}
}
