package fx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
)

func defaultClient(hc *http.Client) *http.Client {
	if hc == nil {
		return &http.Client{Timeout: 10 * time.Second}
	}
	return hc
}

// getJSON fetches a URL and decodes it keeping numbers exact.
func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %d", u, res.StatusCode)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	return nil
}

func ratOf(n json.Number) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(n.String())
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("bad rate %q", n)
	}
	return r, nil
}

// fiat returns the upper case currencies other than USD, de-duplicated.
func fiat(currencies []string) []string {
	var out []string
	for _, c := range currencies {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c != "" && c != "USD" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

type frankfurter struct {
	base       string
	currencies []string
	hc         *http.Client
}

// Frankfurter reads fiat rates: GET {base}/latest?from=USD&to=JPY → {"rates":{"JPY":150.1}} gives USD/JPY.
func Frankfurter(base string, currencies []string, hc *http.Client) Provider {
	cs := fiat(currencies)
	if len(cs) == 0 {
		cs = []string{"JPY"}
	}
	return &frankfurter{base: strings.TrimRight(base, "/"), currencies: cs, hc: defaultClient(hc)}
}

func (f *frankfurter) Name() string { return "frankfurter" }

func (f *frankfurter) Rates(ctx context.Context) (map[string]*big.Rat, error) {
	u := f.base + "/latest?from=USD&to=" + url.QueryEscape(strings.Join(f.currencies, ","))
	var body struct {
		Rates map[string]json.Number `json:"rates"`
	}
	if err := getJSON(ctx, f.hc, u, &body); err != nil {
		return nil, err
	}
	out := map[string]*big.Rat{}
	for c, n := range body.Rates {
		r, err := ratOf(n)
		if err != nil {
			return nil, fmt.Errorf("frankfurter %s: %w", c, err)
		}
		out["USD/"+strings.ToUpper(c)] = r
	}
	if len(out) == 0 {
		return nil, errors.New("frankfurter: no rates")
	}
	return out, nil
}

type mempool struct {
	base string
	hc   *http.Client
}

// Mempool reads BTC prices from mempool.space: GET {base}/api/v1/prices →
// {"time":1790727905,"USD":83464,"JPY":13124019,…} gives BTC/USD, BTC/JPY, …
// (mainnet prices, used as the reference rate for signet orders too).
func Mempool(base string, hc *http.Client) Provider {
	return &mempool{base: strings.TrimRight(base, "/"), hc: defaultClient(hc)}
}

func (m *mempool) Name() string { return "mempool" }

func (m *mempool) Rates(ctx context.Context) (map[string]*big.Rat, error) {
	var body map[string]json.Number
	if err := getJSON(ctx, m.hc, m.base+"/api/v1/prices", &body); err != nil {
		return nil, err
	}
	out := map[string]*big.Rat{}
	for c, n := range body {
		if c == "time" {
			continue
		}
		r, err := ratOf(n)
		if err != nil {
			return nil, fmt.Errorf("mempool %s: %w", c, err)
		}
		out["BTC/"+strings.ToUpper(c)] = r
	}
	if len(out) == 0 {
		return nil, errors.New("mempool: no prices")
	}
	return out, nil
}

type coinGecko struct {
	base string
	vs   []string
	hc   *http.Client
}

// coinGeckoIDs maps CoinGecko ids to our asset symbols.
var coinGeckoIDs = map[string]string{"bitcoin": "BTC", "usd-coin": "USDC"}

// CoinGecko reads crypto prices: GET {base}/api/v3/simple/price?ids=bitcoin,usd-coin&vs_currencies=usd,jpy.
func CoinGecko(base string, hc *http.Client) Provider {
	return CoinGeckoFor(base, []string{"JPY"}, hc)
}

// CoinGeckoFor is CoinGecko with the quote currencies to ask for (USD is always included).
func CoinGeckoFor(base string, currencies []string, hc *http.Client) Provider {
	vs := []string{"usd"}
	for _, c := range fiat(currencies) {
		vs = append(vs, strings.ToLower(c))
	}
	return &coinGecko{base: strings.TrimRight(base, "/"), vs: vs, hc: defaultClient(hc)}
}

func (c *coinGecko) Name() string { return "coingecko" }

func (c *coinGecko) Rates(ctx context.Context) (map[string]*big.Rat, error) {
	u := c.base + "/api/v3/simple/price?ids=bitcoin,usd-coin&vs_currencies=" + strings.Join(c.vs, ",")
	var body map[string]map[string]json.Number
	if err := getJSON(ctx, c.hc, u, &body); err != nil {
		return nil, err
	}
	out := map[string]*big.Rat{}
	for id, prices := range body {
		sym, ok := coinGeckoIDs[id]
		if !ok {
			continue
		}
		for vs, n := range prices {
			r, err := ratOf(n)
			if err != nil {
				return nil, fmt.Errorf("coingecko %s/%s: %w", id, vs, err)
			}
			out[sym+"/"+strings.ToUpper(vs)] = r
		}
	}
	if len(out) == 0 {
		return nil, errors.New("coingecko: no rates")
	}
	return out, nil
}

// Caller runs a view call; *evm.Client satisfies it.
type Caller interface {
	Call(ctx context.Context, contract common.Address, a abi.ABI, method string, args ...any) ([]any, error)
}

type chainlink struct {
	caller Caller
	feeds  map[string]common.Address

	mu       sync.Mutex
	decimals map[common.Address]uint8
}

// Chainlink reads AggregatorV3 feeds; keys of feeds are pairs such as "BTC/USD".
func Chainlink(caller Caller, feeds map[string]common.Address) Provider {
	return &chainlink{caller: caller, feeds: feeds, decimals: map[common.Address]uint8{}}
}

func (c *chainlink) Name() string { return "chainlink" }

func (c *chainlink) feedDecimals(ctx context.Context, feed common.Address) uint8 {
	c.mu.Lock()
	d, ok := c.decimals[feed]
	c.mu.Unlock()
	if ok {
		return d
	}
	d = 8
	if v, err := c.caller.Call(ctx, feed, evm.AggregatorABI, "decimals"); err == nil && len(v) == 1 {
		if n, ok := v[0].(uint8); ok {
			d = n
		}
	}
	c.mu.Lock()
	c.decimals[feed] = d
	c.mu.Unlock()
	return d
}

func (c *chainlink) Rates(ctx context.Context) (map[string]*big.Rat, error) {
	out := map[string]*big.Rat{}
	var errs []error
	for pair, feed := range c.feeds {
		v, err := c.caller.Call(ctx, feed, evm.AggregatorABI, "latestRoundData")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pair, err))
			continue
		}
		answer, ok := v[1].(*big.Int)
		if !ok || answer.Sign() <= 0 {
			errs = append(errs, fmt.Errorf("%s: no positive answer", pair))
			continue
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(c.feedDecimals(ctx, feed))), nil)
		out[strings.ToUpper(pair)] = new(big.Rat).SetFrac(answer, scale)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chainlink: no rates: %w", errors.Join(errs...))
	}
	return out, nil
}

type static struct {
	rates map[string]string
}

// Static returns fixed rates (tests); invalid values are reported by Rates.
func Static(rates map[string]string) Provider { return &static{rates: rates} }

func (s *static) Name() string { return "static" }

func (s *static) Rates(context.Context) (map[string]*big.Rat, error) {
	out := map[string]*big.Rat{}
	for pair, v := range s.rates {
		r, err := ParseRate(v)
		if err != nil {
			return nil, fmt.Errorf("static %s: %w", pair, err)
		}
		out[strings.ToUpper(pair)] = r
	}
	return out, nil
}

// FromConfig builds the providers of the fx.sources configuration.
func FromConfig(sources []config.FXSource, currencies []string, hc *http.Client, caller Caller, deploymentFeeds map[string]common.Address) ([]Provider, error) {
	var out []Provider
	for i, src := range sources {
		switch src.Type {
		case "frankfurter":
			if src.Base == "" {
				return nil, fmt.Errorf("fx source %d: frankfurter needs base", i)
			}
			out = append(out, Frankfurter(src.Base, currencies, hc))
		case "mempool":
			if src.Base == "" {
				return nil, fmt.Errorf("fx source %d: mempool needs base (e.g. https://mempool.space)", i)
			}
			out = append(out, Mempool(src.Base, hc))
		case "coingecko":
			if src.Base == "" {
				return nil, fmt.Errorf("fx source %d: coingecko needs base", i)
			}
			out = append(out, CoinGeckoFor(src.Base, currencies, hc))
		case "chainlink":
			if caller == nil {
				return nil, fmt.Errorf("fx source %d: chainlink needs an EVM RPC", i)
			}
			feeds := deploymentFeeds
			if len(src.Feeds) > 0 {
				feeds = map[string]common.Address{}
				for pair, a := range src.Feeds {
					if !common.IsHexAddress(a) {
						return nil, fmt.Errorf("fx source %d: bad feed address %q", i, a)
					}
					feeds[pair] = common.HexToAddress(a)
				}
			}
			if len(feeds) == 0 {
				return nil, fmt.Errorf("fx source %d: chainlink without feeds", i)
			}
			out = append(out, Chainlink(caller, feeds))
		case "static":
			if _, err := Static(src.Rates).Rates(context.Background()); err != nil {
				return nil, fmt.Errorf("fx source %d: %w", i, err)
			}
			out = append(out, Static(src.Rates))
		default:
			return nil, fmt.Errorf("fx source %d: unknown type %q", i, src.Type)
		}
	}
	return out, nil
}
