package relay

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// limiter is a token bucket per key: rate tokens per minute, up to burst tokens.
type limiter[K comparable] struct {
	perMinute float64
	burst     float64

	mu        sync.Mutex
	buckets   map[K]*bucket
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// newLimiter returns nil (no limit) when perMinute is negative.
func newLimiter[K comparable](perMinute int) *limiter[K] {
	if perMinute < 0 {
		return nil
	}
	return &limiter[K]{perMinute: float64(perMinute), burst: float64(perMinute), buckets: map[K]*bucket{}, now: time.Now}
}

// refill returns the bucket of key with its tokens brought up to date. l.mu is held.
func (l *limiter[K]) refill(key K, now time.Time) *bucket {
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Minutes()*l.perMinute)
	b.at = now
	return b
}

// allow takes a token of key and reports whether there was one.
func (l *limiter[K]) allow(key K) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	b := l.refill(key, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// available reports whether key has a token, without taking it.
func (l *limiter[K]) available(key K) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key, l.now()).tokens >= 1
}

// forget drops the bucket of a key (a closed connection).
func (l *limiter[K]) forget(key K) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweep forgets the buckets that are full again, so the map does not grow with every client ever seen.
func (l *limiter[K]) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Minutes()*l.perMinute >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// proxies is the set of reverse proxies whose X-Forwarded-For is believed.
type proxies []*net.IPNet

func parseProxies(list []string) (proxies, error) {
	var out proxies
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				s += "/32"
			} else {
				s += "/128"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// fromTrusted tells whether the peer of a request is a trusted proxy.
func (p proxies) fromTrusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && p.trusted(ip)
}

func (p proxies) trusted(ip net.IP) bool {
	for _, n := range p {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address of the peer, or — when the peer is a trusted proxy — the last address in
// X-Forwarded-For that is not a trusted proxy. Unlike khatru.GetIPFromRequest it never believes a header sent
// by the client itself, so a client cannot pick its own rate limit bucket.
func (p proxies) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !p.trusted(ip) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := net.ParseIP(strings.TrimSpace(hops[i]))
		if hop == nil {
			break
		}
		if !p.trusted(hop) {
			return hop.String()
		}
	}
	return host
}

// rejectMarker is put in the search field of a filter that OverwriteFilter decided to refuse: khatru calls
// RejectFilter only after OverwriteFilter and not at all for limit:0 filters, so the decision is taken where
// every REQ passes and carried to RejectFilter this way.
const rejectMarker = "\x00psrelay-reject:"

// subscriptions counts the open REQs of every connection. A REQ is one context in khatru (all its filters share
// it), which ends on CLOSE, on a rejected filter and on disconnect.
type subscriptions struct {
	mu   sync.Mutex
	open map[*khatru.WebSocket]map[context.Context]struct{}
}

// add counts the REQ of ctx and reports whether the connection stays within max.
func (s *subscriptions) add(ctx context.Context, max int) bool {
	ws := khatru.GetConnection(ctx)
	if ws == nil || max < 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reqs := s.open[ws]
	if _, ok := reqs[ctx]; ok {
		return true
	}
	if len(reqs) >= max {
		return false
	}
	if reqs == nil {
		reqs = map[context.Context]struct{}{}
		s.open[ws] = reqs
	}
	reqs[ctx] = struct{}{}
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.open[ws], ctx)
		if len(s.open[ws]) == 0 {
			delete(s.open, ws)
		}
	}()
	return true
}

func (s *subscriptions) count(ws *khatru.WebSocket) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open[ws])
}

// addressKey is the rate limit key of a client address: the IPv4 address, or the /64 prefix of an IPv6 address
// (one subscriber usually holds a whole /64, so per-address IPv6 limits would be no limits).
func addressKey(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
}

func (s *Server) ipOf(ctx context.Context) string {
	if ws := khatru.GetConnection(ctx); ws != nil {
		return addressKey(s.proxies.clientIP(ws.Request))
	}
	return ""
}

// rejectConnection limits new WebSocket connections per client address. It also warns (once) when a peer that is
// not a trusted proxy sends X-Forwarded-For: the relay is probably behind a proxy that -trusted-proxies does not
// name, so every client shares the proxy's limits.
func (s *Server) rejectConnection(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" && !s.proxies.fromTrusted(r) && s.xffWarned.CompareAndSwap(false, true) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.log.Error("X-Forwarded-For from a peer that is not a trusted proxy: if the relay is behind a reverse proxy, "+
			"set -trusted-proxies to its address, or all clients share its rate limits", "peer", host)
	}
	return !s.connLimit.allow(addressKey(s.proxies.clientIP(r)))
}

// limitFrame is the per-connection frame limit, applied before khatru parses a frame or verifies a signature.
func (s *Server) limitFrame(ctx context.Context) bool {
	ws := khatru.GetConnection(ctx)
	return ws != nil && !s.connFrames.allow(ws)
}

// forgetConnection drops the per-connection buckets of a closed connection.
func (s *Server) forgetConnection(ctx context.Context) {
	if ws := khatru.GetConnection(ctx); ws != nil {
		s.connFrames.forget(ws)
		s.connEvents.forget(ws)
	}
}

// limitEvent applies the per-address and per-connection event limits.
func (s *Server) limitEvent(ctx context.Context, _ *nostr.Event) (bool, string) {
	ws := khatru.GetConnection(ctx)
	if ws == nil {
		return false, "" // internal call
	}
	if !s.connEvents.allow(ws) || !s.ipEvents.allow(s.ipOf(ctx)) {
		return true, "rate-limited: too many events, slow down"
	}
	return false, ""
}

// overwriteFilter caps the limit and applies the REQ limits (see rejectMarker).
func (s *Server) overwriteFilter(ctx context.Context, f *nostr.Filter) {
	if khatru.GetConnection(ctx) == nil {
		return // internal query (deletions, replacements)
	}
	if s.opts.MaxLimit > 0 && (f.Limit <= 0 || f.Limit > s.opts.MaxLimit) && !f.LimitZero {
		f.Limit = s.opts.MaxLimit
	}
	reason := ""
	switch {
	case f.Search != "":
		reason = "unsupported: search is not supported"
	case !s.ipFilters.allow(s.ipOf(ctx)):
		reason = "rate-limited: too many subscriptions, slow down"
	case !s.subs.add(ctx, s.opts.MaxSubscriptions):
		reason = "blocked: too many open subscriptions on this connection"
	}
	if reason != "" {
		f.LimitZero = false
		f.Search = rejectMarker + reason
	}
}

func (s *Server) rejectFilter(_ context.Context, f nostr.Filter) (bool, string) {
	if reason, ok := strings.CutPrefix(f.Search, rejectMarker); ok {
		return true, reason
	}
	return false, ""
}

// deletionOutcome replaces khatru's decision on a kind 5 target: khatru deletes before it runs RejectEvent on the
// deletion itself, so the kind and rate policies are applied here as well. Only the author deletes (NIP-09).
func (s *Server) deletionOutcome(ctx context.Context, target, deletion *nostr.Event) (bool, string) {
	switch {
	case target.PubKey != deletion.PubKey:
		return false, "you are not the author of this event"
	case !s.accepts(deletion.Kind):
		return false, "kind 5 is not accepted here"
	}
	if ws := khatru.GetConnection(ctx); ws != nil && (!s.connEvents.available(ws) || !s.ipEvents.available(s.ipOf(ctx))) {
		return false, "rate-limited: too many events, slow down"
	}
	return true, ""
}
