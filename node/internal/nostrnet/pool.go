// Package nostrnet keeps connections to Nostr relays: publishing to several relays, long running
// subscriptions that reconnect, and one-shot queries.
package nostrnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// Defaults of the pool.
const (
	DefaultIdleTimeout    = 5 * time.Minute
	DefaultPublishTimeout = 15 * time.Second
	// DefaultMaxConns caps the open relay connections of a pool (relays named by peers included).
	DefaultMaxConns = 64
	connectTimeout  = 10 * time.Second
	// MaxQueryEvents caps the events one relay may return to a query (the filter's limit when smaller).
	MaxQueryEvents = 5000
	// stableSubscription: a subscription that lasted this long resets the reconnect backoff.
	stableSubscription = time.Minute
)

// ErrTooManyConns is returned when the pool holds its maximum of connections and none is idle.
var ErrTooManyConns = errors.New("too many relay connections")

// ErrPrivateAddress is returned for relays on loopback, link-local or private addresses when they are not allowed.
var ErrPrivateAddress = errors.New("relay address is loopback, link-local or private")

// Options configure a pool.
type Options struct {
	TLS *tls.Config // nil: the system roots
	Log *slog.Logger
	// AllowPrivate permits relays on loopback, link-local, private and unique-local addresses (§4.10).
	// The address is checked when the socket connects, so a name resolving elsewhere later does not get around it.
	AllowPrivate bool
	// IdleTimeout closes connections that had no subscription and no request for this long (default 5 minutes).
	IdleTimeout time.Duration
	// PublishTimeout bounds the wait for one relay's OK (default 15 seconds); the caller's context may end it sooner.
	PublishTimeout time.Duration
	// MaxConns caps the open connections (default DefaultMaxConns). At the cap, idle connections are closed to
	// make room; without an idle one a new relay is refused with ErrTooManyConns.
	MaxConns int
}

// Pool is a set of relay connections sharing one TLS configuration.
type Pool struct {
	opts Options
	log  *slog.Logger
	hc   *http.Client

	mu     sync.Mutex
	conns  map[string]*conn
	dials  map[string]chan struct{} // dials in progress
	closed bool
	stop   chan struct{}
}

// NewPool returns an empty pool that connects to any address (tools and tests); tc may be nil for the system
// roots. Nodes use NewPoolWith and the configured private-address policy.
func NewPool(tc *tls.Config, log *slog.Logger) *Pool {
	return NewPoolWith(Options{TLS: tc, Log: log, AllowPrivate: true})
}

// NewPoolWith returns an empty pool.
func NewPoolWith(o Options) *Pool {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = DefaultPublishTimeout
	}
	if o.MaxConns <= 0 {
		o.MaxConns = DefaultMaxConns
	}
	d := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	if !o.AllowPrivate {
		d.Control = refusePrivate
	}
	// no HTTP/2: the WebSocket upgrade needs HTTP/1.1
	tr := &http.Transport{TLSClientConfig: o.TLS, DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: connectTimeout}
	p := &Pool{
		opts: o, log: o.Log, hc: &http.Client{Transport: tr},
		conns: map[string]*conn{}, dials: map[string]chan struct{}{}, stop: make(chan struct{}),
	}
	go p.evictLoop()
	return p
}

// refusePrivate is the dialer's Control hook: it sees the address actually connected to.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, address)
	}
	if IsPrivateAddr(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, address)
	}
	return nil
}

// IsPrivateAddr tells whether an address is loopback, link-local, private (RFC 1918, ULA), CGNAT or unspecified.
func IsPrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsUnspecified() || a.IsMulticast() || cgnat.Contains(a)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Allowed tells whether the pool would connect to a relay URL as far as the URL shows: a ws / wss URL with a host,
// and — unless private addresses are allowed — not localhost or a literal private address. Names are checked
// again when the socket connects. Callers filter relay lists with it before taking the first few (§4.10), so that
// unusable entries do not use up the places.
func (p *Pool) Allowed(rawURL string) bool {
	pu, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (pu.Scheme != "ws" && pu.Scheme != "wss") || pu.Hostname() == "" {
		return false
	}
	if p.opts.AllowPrivate {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(pu.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if a, err := netip.ParseAddr(host); err == nil && IsPrivateAddr(a) {
		return false
	}
	return true
}

// relay returns a live connection, dialing it if needed. Concurrent callers for one URL share one dial, so a
// connection is never closed while it is still being set up.
func (p *Pool) relay(ctx context.Context, rawURL string) (*conn, error) {
	u := nostr.NormalizeURL(rawURL)
	if pu, err := url.Parse(u); err != nil || (pu.Scheme != "ws" && pu.Scheme != "wss") || pu.Host == "" {
		return nil, fmt.Errorf("bad relay url %q", rawURL)
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool closed")
		}
		if c := p.conns[u]; c != nil && c.alive() {
			p.mu.Unlock()
			return c, nil
		}
		if wait, dialing := p.dials[u]; dialing {
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if len(p.conns)+len(p.dials) >= p.opts.MaxConns {
			idle := p.makeRoomLocked()
			full := len(p.conns)+len(p.dials) >= p.opts.MaxConns
			p.mu.Unlock()
			if idle != nil {
				idle.close(errors.New("closed to make room"))
			}
			if full {
				return nil, fmt.Errorf("connect %s: %w (%d)", u, ErrTooManyConns, p.opts.MaxConns)
			}
			continue
		}
		done := make(chan struct{})
		p.dials[u] = done
		p.mu.Unlock()

		cctx, cancel := context.WithTimeout(ctx, connectTimeout)
		c, err := dial(cctx, u, p.hc, p.log)
		cancel()

		p.mu.Lock()
		delete(p.dials, u)
		close(done)
		if err == nil {
			if p.closed {
				p.mu.Unlock()
				c.close(errors.New("pool closed"))
				return nil, errors.New("pool closed")
			}
			p.conns[u] = c
		}
		p.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("connect %s: %w", u, err)
		}
		return c, nil
	}
}

// makeRoomLocked drops a dead connection, or the connection idle for the longest time, from the map and returns
// it for closing (nil when every connection is in use). p.mu is held.
func (p *Pool) makeRoomLocked() *conn {
	var victim string
	var oldest time.Time
	for u, c := range p.conns {
		if !c.alive() {
			victim = u
			break
		}
		since := c.idleSince()
		if !since.IsZero() && (victim == "" || since.Before(oldest)) {
			victim, oldest = u, since
		}
	}
	if victim == "" {
		return nil
	}
	c := p.conns[victim]
	delete(p.conns, victim)
	return c
}

// Close closes all connections.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	conns := p.conns
	p.conns = map[string]*conn{}
	p.mu.Unlock()
	for _, c := range conns {
		c.close(errors.New("pool closed"))
	}
}

// evictLoop closes connections that nobody used for the idle timeout (relays named once by a peer, say).
func (p *Pool) evictLoop() {
	t := time.NewTicker(max(p.opts.IdleTimeout/4, time.Second))
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		p.evictIdle(time.Now().Add(-p.opts.IdleTimeout))
	}
}

func (p *Pool) evictIdle(before time.Time) int {
	var idle []*conn
	p.mu.Lock()
	for u, c := range p.conns {
		since := c.idleSince()
		if !c.alive() || (!since.IsZero() && since.Before(before)) {
			delete(p.conns, u)
			idle = append(idle, c)
		}
	}
	p.mu.Unlock()
	for _, c := range idle {
		c.close(errors.New("idle"))
	}
	return len(idle)
}

// Connections returns how many relay connections are open.
func (p *Pool) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// Publish sends an event to the relays concurrently and returns the relays that accepted it. Each relay gets at
// most the pool's publish timeout, and ctx can end the wait sooner.
func (p *Pool) Publish(ctx context.Context, urls []string, ev *nostr.Event) ([]string, error) {
	type result struct {
		url string
		err error
	}
	ch := make(chan result, len(urls))
	for _, u := range urls {
		go func(u string) {
			pctx, cancel := context.WithTimeout(ctx, p.opts.PublishTimeout)
			defer cancel()
			c, err := p.relay(pctx, u)
			if err == nil {
				err = c.publish(pctx, ev)
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
			c, err := p.relay(qctx, u)
			if err != nil {
				p.log.Debug("query: relay unavailable", "relay", u, "err", err)
				return
			}
			evs, err := c.query(qctx, filter)
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
//
// A reconnect asks for the same filters again, without a since: gift wraps keep the created_at of their first
// sending, so a message that reached the relay while we were away can be older than any since we could choose.
// Stored events come again and the handler de-duplicates them by id; filters should carry a limit.
func (p *Pool) Subscribe(ctx context.Context, urls []string, filters nostr.Filters, handle func(relay string, ev *nostr.Event)) {
	for _, u := range urls {
		go p.keepSubscribed(ctx, u, filters, handle)
	}
}

func (p *Pool) keepSubscribed(ctx context.Context, url string, filters nostr.Filters, handle func(string, *nostr.Event)) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := p.relay(ctx, url)
		var sub *subscription
		if err == nil {
			sub, err = c.subscribe(ctx, filters)
		}
		if err != nil {
			p.log.Debug("subscribe failed", "relay", url, "err", err)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		p.log.Debug("subscribed", "relay", url)
		started := time.Now()
	loop:
		for {
			select {
			case ev := <-sub.events:
				handle(url, ev)
			case <-sub.done:
				break loop
			case <-ctx.Done():
				sub.unsub()
				return
			}
		}
		// a relay closing the subscription at once (CLOSED: rate limited, refused filter) is retried with a growing
		// delay; only a subscription that lasted resets it
		if time.Since(started) >= stableSubscription {
			backoff = time.Second
		}
		p.log.Debug("subscription ended, reconnecting", "relay", url, "err", sub.err, "in", backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
