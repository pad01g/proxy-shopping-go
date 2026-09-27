// Package fx fetches exchange rates from interchangeable providers (spec §7), synthesizes missing pairs through
// USD and takes the median across providers.
package fx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider is a source of rates. The keys of Rates are pairs "BASE/QUOTE": one BASE costs rate QUOTE.
type Provider interface {
	Name() string
	Rates(ctx context.Context) (map[string]*big.Rat, error)
}

// SourceRate is the rate one provider gave for a pair.
type SourceRate struct {
	Name string
	Rate *big.Rat
	At   int64
}

// Quote is the median rate of a pair.
type Quote struct {
	Pair    string
	Rate    *big.Rat
	Sources []SourceRate
	At      int64
}

// cacheTTL bounds how long a quote is reused.
const cacheTTL = 3 * time.Second

// Service combines providers.
type Service struct {
	providers []Provider
	log       *slog.Logger
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]*Quote
}

// New returns a service over the providers.
func New(providers []Provider, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{providers: providers, log: log.With("component", "fx"), now: time.Now, cache: map[string]*Quote{}}
}

// Quote returns the median rate of pair across the providers that can produce it.
func (s *Service) Quote(ctx context.Context, pair string) (*Quote, error) {
	base, quote, err := splitPair(pair)
	if err != nil {
		return nil, err
	}
	pair = base + "/" + quote
	now := s.now()
	s.mu.Lock()
	if q, ok := s.cache[pair]; ok && now.Sub(time.Unix(q.At, 0)) < cacheTTL {
		s.mu.Unlock()
		return q, nil
	}
	s.mu.Unlock()

	type result struct {
		name  string
		rates map[string]*big.Rat
		err   error
	}
	results := make([]result, len(s.providers))
	var wg sync.WaitGroup
	for i, p := range s.providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			rates, err := p.Rates(ctx)
			results[i] = result{p.Name(), rates, err}
		}(i, p)
	}
	wg.Wait()

	var sources []SourceRate
	var errs []error
	for _, r := range results {
		if r.err != nil {
			s.log.Warn("rate source failed", "source", r.name, "err", r.err)
			errs = append(errs, fmt.Errorf("%s: %w", r.name, r.err))
			continue
		}
		rate, ok := Derive(r.rates, base, quote)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no rate for %s", r.name, pair))
			continue
		}
		sources = append(sources, SourceRate{Name: r.name, Rate: rate, At: now.Unix()})
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no rate for %s: %w", pair, errors.Join(errs...))
	}
	rates := make([]*big.Rat, len(sources))
	for i, src := range sources {
		rates[i] = src.Rate
	}
	q := &Quote{Pair: pair, Rate: Median(rates), Sources: sources, At: now.Unix()}
	s.mu.Lock()
	s.cache[pair] = q
	s.mu.Unlock()
	return q, nil
}

func splitPair(pair string) (string, string, error) {
	base, quote, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(pair)), "/")
	if !ok || base == "" || quote == "" || base == quote {
		return "", "", fmt.Errorf("bad pair %q", pair)
	}
	return base, quote, nil
}

// Derive finds base/quote in a rate table: directly, inverted, or through USD.
func Derive(rates map[string]*big.Rat, base, quote string) (*big.Rat, bool) {
	if base == quote {
		return big.NewRat(1, 1), true
	}
	if r := rates[base+"/"+quote]; positive(r) {
		return new(big.Rat).Set(r), true
	}
	if r := rates[quote+"/"+base]; positive(r) {
		return new(big.Rat).Inv(r), true
	}
	if base == "USD" || quote == "USD" {
		return nil, false
	}
	a, ok := direct(rates, base, "USD")
	if !ok {
		return nil, false
	}
	b, ok := direct(rates, "USD", quote)
	if !ok {
		return nil, false
	}
	return new(big.Rat).Mul(a, b), true
}

// direct is Derive without the USD detour.
func direct(rates map[string]*big.Rat, base, quote string) (*big.Rat, bool) {
	if r := rates[base+"/"+quote]; positive(r) {
		return r, true
	}
	if r := rates[quote+"/"+base]; positive(r) {
		return new(big.Rat).Inv(r), true
	}
	return nil, false
}

func positive(r *big.Rat) bool { return r != nil && r.Sign() > 0 }

// Median of the rates; with an even count the mean of the two middle values.
func Median(rates []*big.Rat) *big.Rat {
	sorted := append([]*big.Rat(nil), rates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cmp(sorted[j]) < 0 })
	n := len(sorted)
	if n == 0 {
		return nil
	}
	if n%2 == 1 {
		return new(big.Rat).Set(sorted[n/2])
	}
	sum := new(big.Rat).Add(sorted[n/2-1], sorted[n/2])
	return sum.Quo(sum, big.NewRat(2, 1))
}

// rateDecimals is the precision of FormatRate.
const rateDecimals = 8

// FormatRate writes a rate as a decimal string with at most 8 fraction digits (rounded half up), without
// trailing zeros.
func FormatRate(r *big.Rat) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(rateDecimals), nil)
	n := RoundHalfUp(new(big.Rat).Mul(r, new(big.Rat).SetInt(scale)))
	neg := n.Sign() < 0
	n.Abs(n)
	s := n.String()
	if len(s) <= rateDecimals {
		s = strings.Repeat("0", rateDecimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-rateDecimals], strings.TrimRight(s[len(s)-rateDecimals:], "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

// RoundHalfUp rounds to the nearest integer, halves away from zero.
func RoundHalfUp(r *big.Rat) *big.Int {
	num := new(big.Int).Abs(r.Num())
	den := r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if r.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// ParseRate parses a positive decimal rate.
func ParseRate(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	r, ok := new(big.Rat).SetString(s)
	if !ok || s == "" || strings.ContainsAny(s, "/eE") {
		return nil, fmt.Errorf("bad rate %q", s)
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("rate %q is not positive", s)
	}
	return r, nil
}
