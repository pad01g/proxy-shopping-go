package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/nbd-wtf/go-nostr"
)

// MaxBundleBytes and MaxBundleEvents bound a bundle (§2.6: 2 MiB, 1000 events). A larger body is refused; events
// past the limit are not looked at.
var (
	MaxBundleBytes  int64 = 2 << 20
	MaxBundleEvents       = 1000
)

// FetchBundle downloads a bundle (§2.6): a JSON file of signed events, {"events": [...]} (the events.json of a
// registry such as pad01g/proxy-shopping-registry, or the bundle an operator keeps at its list_url) or a plain
// array. Only https URLs are fetched, and redirects only within the same origin. It does not verify the events:
// a bundle adds no trust of its own, every event is checked by Put like one from a relay.
func FetchBundle(ctx context.Context, client *http.Client, rawURL string) ([]*nostr.Event, error) {
	origin, err := url.Parse(rawURL)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return nil, fmt.Errorf("bundle %s: not an https URL", rawURL)
	}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) {
			return fmt.Errorf("redirect to another origin (%s://%s)", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bundle %s: HTTP %d", rawURL, res.StatusCode)
	}
	if res.ContentLength > MaxBundleBytes {
		return nil, fmt.Errorf("bundle %s: larger than %d bytes", rawURL, MaxBundleBytes)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", rawURL, err)
	}
	if int64(len(data)) > MaxBundleBytes {
		return nil, fmt.Errorf("bundle %s: larger than %d bytes", rawURL, MaxBundleBytes)
	}
	return ParseBundle(data)
}

// Bundle is the file format of §2.6.
type Bundle struct {
	Events []*nostr.Event `json:"events"`
}

// MakeBundle orders events for a bundle (§2.6): delegations, lists, then profiles and inbox relays.
func MakeBundle(evs []*nostr.Event) *Bundle {
	out := make([]*nostr.Event, 0, len(evs))
	for _, ev := range evs {
		if ev != nil {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return syncRank(out[i].Kind) < syncRank(out[j].Kind) })
	return &Bundle{Events: out}
}

// ParseBundle decodes the events of a trust bundle ({"events": [...]} or [...]).
func ParseBundle(data []byte) ([]*nostr.Event, error) {
	// events are decoded one by one, so that one malformed event does not cost the others (§2.6)
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		var obj struct {
			Events []json.RawMessage `json:"events"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || obj.Events == nil {
			return nil, errors.New("bundle: expected {\"events\": [...]} or an array of events")
		}
		raw = obj.Events
	}
	if len(raw) > MaxBundleEvents {
		raw = raw[:MaxBundleEvents]
	}
	evs := make([]*nostr.Event, 0, len(raw))
	for _, r := range raw {
		var ev nostr.Event
		if json.Unmarshal(r, &ev) == nil && ev.ID != "" {
			evs = append(evs, &ev)
		}
	}
	return evs, nil
}

// PutBundle stores the events of a bundle of network, delegations first, then lists, then profiles (so that the
// scope has grown to their authors when they come), and returns the ones that were new and newer. Events that do
// not verify or belong to another network are skipped; events outside the scope are parked, as from a relay.
func (s *Store) PutBundle(evs []*nostr.Event, network string) (stored []*nostr.Event, rejected int) {
	sorted := make([]*nostr.Event, len(evs))
	copy(sorted, evs)
	sort.SliceStable(sorted, func(i, j int) bool { return syncRank(sorted[i].Kind) < syncRank(sorted[j].Kind) })
	for _, ev := range sorted {
		if n := Network(ev); n != "" && n != network && ev.Kind != KindInboxRelays {
			rejected++
			continue
		}
		newer, err := s.Put(ev)
		switch {
		case err != nil && !errors.Is(err, ErrOutOfScope):
			rejected++
		case newer:
			stored = append(stored, ev)
		}
	}
	return stored, rejected
}
