// Package nostrnet keeps connections to Nostr relays: publishing to several relays, long running
// subscriptions that reconnect, and one-shot queries.
package nostrnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Pool is a set of relay connections sharing one TLS configuration.
type Pool struct {
	tls *tls.Config
	log *slog.Logger

	mu     sync.Mutex
	relays map[string]*nostr.Relay
}

// NewPool returns an empty pool; tc may be nil for the system roots.
func NewPool(tc *tls.Config, log *slog.Logger) *Pool {
	if log == nil {
		log = slog.Default()
	}
	return &Pool{tls: tc, log: log, relays: map[string]*nostr.Relay{}}
}

// Relay returns a connected relay, dialing it if needed.
func (p *Pool) Relay(ctx context.Context, url string) (*nostr.Relay, error) {
	url = nostr.NormalizeURL(url)
	p.mu.Lock()
	r := p.relays[url]
	p.mu.Unlock()
	if r != nil && r.IsConnected() {
		return r, nil
	}
	r = nostr.NewRelay(context.Background(), url)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.ConnectWithTLS(cctx, p.tls); err != nil {
		return nil, fmt.Errorf("connect %s: %w", url, err)
	}
	p.mu.Lock()
	if old := p.relays[url]; old != nil && old != r && old.IsConnected() {
		p.mu.Unlock()
		_ = r.Close()
		return old, nil
	}
	p.relays[url] = r
	p.mu.Unlock()
	return r, nil
}

// Close closes all connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.relays {
		_ = r.Close()
	}
	p.relays = map[string]*nostr.Relay{}
}

// Publish sends an event to the relays concurrently and returns the relays that accepted it.
func (p *Pool) Publish(ctx context.Context, urls []string, ev *nostr.Event) ([]string, error) {
	type result struct {
		url string
		err error
	}
	ch := make(chan result, len(urls))
	for _, u := range urls {
		go func(u string) {
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			r, err := p.Relay(pctx, u)
			if err == nil {
				err = r.Publish(pctx, *ev)
			}
			ch <- result{u, err}
		}(u)
	}
	var ok []string
	var errs []error
	for range urls {
		res := <-ch
		if res.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.url, res.err))
			continue
		}
		ok = append(ok, res.url)
	}
	return ok, errors.Join(errs...)
}

// Query fetches the stored events matching the filter from all relays (until EOSE), without duplicates.
func (p *Pool) Query(ctx context.Context, urls []string, filter nostr.Filter) []*nostr.Event {
	var mu sync.Mutex
	seen := map[string]bool{}
	var out []*nostr.Event
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			r, err := p.Relay(qctx, u)
			if err != nil {
				p.log.Debug("query: relay unavailable", "relay", u, "err", err)
				return
			}
			evs, err := r.QuerySync(qctx, filter)
			if err != nil {
				p.log.Debug("query failed", "relay", u, "err", err)
			}
			mu.Lock()
			for _, ev := range evs {
				if !seen[ev.ID] {
					seen[ev.ID] = true
					out = append(out, ev)
				}
			}
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return out
}

// Subscribe keeps a subscription open on every relay until ctx ends, reconnecting after failures.
// After a reconnect only events of the last minutes before the drop are asked for again.
func (p *Pool) Subscribe(ctx context.Context, urls []string, filters nostr.Filters, handle func(relay string, ev *nostr.Event)) {
	for _, u := range urls {
		go p.keepSubscribed(ctx, u, filters, handle)
	}
}

func (p *Pool) keepSubscribed(ctx context.Context, url string, filters nostr.Filters, handle func(string, *nostr.Event)) {
	backoff := time.Second
	var since *nostr.Timestamp
	for ctx.Err() == nil {
		r, err := p.Relay(ctx, url)
		if err != nil {
			p.log.Debug("subscribe: relay unavailable", "relay", url, "err", err)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		fs := make(nostr.Filters, len(filters))
		for i, f := range filters {
			fs[i] = f
			if since != nil {
				fs[i].Since = since
			}
		}
		sub, err := r.Subscribe(ctx, fs)
		if err != nil {
			p.log.Debug("subscribe failed", "relay", url, "err", err)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		p.log.Debug("subscribed", "relay", url)
	loop:
		for {
			select {
			case ev, ok := <-sub.Events:
				if !ok {
					break loop
				}
				handle(url, ev)
			case <-sub.Context.Done():
				break loop
			case <-ctx.Done():
				sub.Unsub()
				return
			}
		}
		ts := nostr.Timestamp(time.Now().Add(-10 * time.Minute).Unix())
		since = &ts
		p.log.Debug("subscription ended, reconnecting", "relay", url)
		sleep(ctx, backoff)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
