// Package rates imitates the Frankfurter and CoinGecko APIs for rates.test.
package rates

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
)

// The three base pairs the lab controls (docs/lab.md "lab の値").
// Everything else is derived from them.
const (
	BTCUSD  = "BTC/USD"
	USDJPY  = "USD/JPY"
	USDCUSD = "USDC/USD"
)

// Defaults are the lab values.
var Defaults = map[string]string{BTCUSD: "100000", USDJPY: "150", USDCUSD: "1"}

type Store struct {
	mu    sync.RWMutex
	rates map[string]*big.Rat
}

func NewStore() *Store {
	s := &Store{rates: map[string]*big.Rat{}}
	if err := s.Set(Defaults); err != nil {
		panic(err)
	}
	return s
}

// Set updates some or all of the base pairs. Values are positive decimal
// strings. Nothing is changed if any value is invalid.
func (s *Store) Set(values map[string]string) error {
	parsed := make(map[string]*big.Rat, len(values))
	for pair, v := range values {
		if _, ok := Defaults[pair]; !ok {
			return fmt.Errorf("unknown pair %q (want one of %s)", pair, strings.Join(pairs(), ", "))
		}
		r, ok := new(big.Rat).SetString(v)
		if !ok || r.Sign() <= 0 || strings.ContainsAny(v, "/eE") {
			return fmt.Errorf("invalid rate %q for %s", v, pair)
		}
		parsed[pair] = r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for pair, r := range parsed {
		s.rates[pair] = r
	}
	return nil
}

// Snapshot returns the base pairs as decimal strings.
func (s *Store) Snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.rates))
	for pair, r := range s.rates {
		out[pair] = decimal(r)
	}
	return out
}

// usdPrice returns the price of one unit of asset in USD.
// Assets: USD, JPY, BTC, USDC.
func (s *Store) usdPrice(asset string) (*big.Rat, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch asset {
	case "USD":
		return big.NewRat(1, 1), true
	case "JPY":
		return new(big.Rat).Inv(s.rates[USDJPY]), true
	case "BTC":
		return new(big.Rat).Set(s.rates[BTCUSD]), true
	case "USDC":
		return new(big.Rat).Set(s.rates[USDCUSD]), true
	}
	return nil, false
}

// Rate returns how many units of quote one unit of base buys.
func (s *Store) Rate(base, quote string) (*big.Rat, bool) {
	b, ok1 := s.usdPrice(base)
	q, ok2 := s.usdPrice(quote)
	if !ok1 || !ok2 {
		return nil, false
	}
	return new(big.Rat).Quo(b, q), true
}

func pairs() []string {
	out := make([]string, 0, len(Defaults))
	for p := range Defaults {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// decimal renders r with up to 10 fraction digits and no trailing zeros.
func decimal(r *big.Rat) string {
	s := r.FloatString(10)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
