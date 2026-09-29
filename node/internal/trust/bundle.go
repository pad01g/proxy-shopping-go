package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/nbd-wtf/go-nostr"
)

// MaxBundleBytes bounds the size of a trust bundle.
var MaxBundleBytes int64 = 8 << 20

// MaxBundleEvents bounds the events taken from one bundle.
const MaxBundleEvents = 5000

// FetchBundle downloads a trust bundle: a JSON file of signed events, either {"events": [...]} (the events.json of
// a registry such as pad01g/proxy-shopping-registry) or a plain array. It does not verify the events: a bundle adds
// no trust of its own, every event is checked by Put like one from a relay.
func FetchBundle(ctx context.Context, client *http.Client, url string) ([]*nostr.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trust bundle %s: HTTP %d", url, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("trust bundle %s: %w", url, err)
	}
	if int64(len(data)) > MaxBundleBytes {
		return nil, fmt.Errorf("trust bundle %s: larger than %d bytes", url, MaxBundleBytes)
	}
	return ParseBundle(data)
}

// ParseBundle decodes the events of a trust bundle ({"events": [...]} or [...]).
func ParseBundle(data []byte) ([]*nostr.Event, error) {
	var evs []*nostr.Event
	if err := json.Unmarshal(data, &evs); err != nil {
		var obj struct {
			Events []*nostr.Event `json:"events"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || obj.Events == nil {
			return nil, errors.New("trust bundle: expected {\"events\": [...]} or an array of events")
		}
		evs = obj.Events
	}
	if len(evs) > MaxBundleEvents {
		evs = evs[:MaxBundleEvents]
	}
	out := evs[:0]
	for _, ev := range evs {
		if ev != nil {
			out = append(out, ev)
		}
	}
	return out, nil
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
