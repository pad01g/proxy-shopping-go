package fx

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
)

func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

// labServer answers like the lab ratemock.
func labServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/frankfurter/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("from") != "USD" || r.URL.Query().Get("to") != "JPY" {
			http.Error(w, "bad query", 400)
			return
		}
		_, _ = w.Write([]byte(`{"amount":1.0,"base":"USD","date":"2026-09-27","rates":{"JPY":150}}`))
	})
	mux.HandleFunc("/coingecko/api/v3/simple/price", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ids") != "bitcoin,usd-coin" || r.URL.Query().Get("vs_currencies") != "usd,jpy" {
			http.Error(w, "bad query", 400)
			return
		}
		_, _ = w.Write([]byte(`{"bitcoin":{"usd":100000,"jpy":15000000},"usd-coin":{"usd":1.0,"jpy":150.0}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLabProviders(t *testing.T) {
	srv := labServer(t)
	ps, err := FromConfig([]config.FXSource{
		{Type: "coingecko", Base: srv.URL + "/coingecko"},
		{Type: "frankfurter", Base: srv.URL + "/frankfurter"},
	}, []string{"JPY", "USD"}, srv.Client(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(ps, nil)
	ctx := context.Background()
	for pair, want := range map[string]string{
		"BTC/JPY": "15000000", "BTC/USD": "100000", "USDC/USD": "1", "USDC/JPY": "150", "USD/JPY": "150", "JPY/USD": "0.00666667",
	} {
		q, err := s.Quote(ctx, pair)
		if err != nil {
			t.Fatalf("%s: %v", pair, err)
		}
		if got := FormatRate(q.Rate); got != want {
			t.Errorf("%s = %s, want %s (sources %+v)", pair, got, want, q.Sources)
		}
	}
	// frankfurter has no BTC: only coingecko answers BTC/JPY
	q, _ := s.Quote(ctx, "BTC/JPY")
	if len(q.Sources) != 1 || q.Sources[0].Name != "coingecko" {
		t.Fatalf("sources %+v", q.Sources)
	}
	// coingecko has no fiat cross rate: only frankfurter answers USD/JPY
	q, _ = s.Quote(ctx, "usd/jpy")
	if len(q.Sources) != 1 || q.Sources[0].Name != "frankfurter" || q.Pair != "USD/JPY" {
		t.Fatalf("sources %+v", q.Sources)
	}
}

// TestMempool reads mempool.space /api/v1/prices: BTC in several fiat currencies, "time" is not a price.
func TestMempool(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/prices", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"time":1790727905,"USD":83464,"EUR":73622,"JPY":13124019}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ps, err := FromConfig([]config.FXSource{{Type: "mempool", Base: srv.URL}}, []string{"JPY"}, srv.Client(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(ps, nil)
	for pair, want := range map[string]string{"BTC/USD": "83464", "BTC/JPY": "13124019", "BTC/EUR": "73622"} {
		q, err := s.Quote(context.Background(), pair)
		if err != nil {
			t.Fatalf("%s: %v", pair, err)
		}
		if got := FormatRate(q.Rate); got != want || q.Sources[0].Name != "mempool" {
			t.Errorf("%s = %s from %+v, want %s", pair, got, q.Sources, want)
		}
	}
	if _, err := FromConfig([]config.FXSource{{Type: "mempool"}}, nil, nil, nil, nil); err == nil {
		t.Fatal("mempool without base accepted")
	}
}

func TestSynthesisAndInverse(t *testing.T) {
	rates := map[string]*big.Rat{"BTC/USD": rat("100000"), "JPY/USD": rat("1/150"), "USDC/USD": rat("1")}
	for _, c := range []struct{ base, quote, want string }{
		{"BTC", "JPY", "15000000"}, {"USD", "JPY", "150"}, {"USD", "BTC", "0.00001"}, {"USDC", "JPY", "150"}, {"JPY", "BTC", "0.00000007"},
	} {
		r, ok := Derive(rates, c.base, c.quote)
		if !ok || FormatRate(r) != c.want {
			t.Errorf("%s/%s = %v, want %s", c.base, c.quote, r, c.want)
		}
	}
	if _, ok := Derive(rates, "EUR", "JPY"); ok {
		t.Error("EUR/JPY derived from nothing")
	}
}

func TestMedian(t *testing.T) {
	s := func(p string, v string) Provider { return namedStatic{p, map[string]string{"BTC/USD": v}} }
	ctx := context.Background()
	for _, c := range []struct {
		ps   []Provider
		want string
	}{
		{[]Provider{s("a", "100")}, "100"},
		{[]Provider{s("a", "100"), s("b", "103")}, "101.5"},
		{[]Provider{s("a", "100"), s("b", "200"), s("c", "101")}, "101"},
	} {
		q, err := New(c.ps, nil).Quote(ctx, "BTC/USD")
		if err != nil || FormatRate(q.Rate) != c.want {
			t.Errorf("median %v, want %s (%v)", q, c.want, err)
		}
	}
	if _, err := New([]Provider{failing{}}, nil).Quote(ctx, "BTC/USD"); err == nil {
		t.Error("quote without sources")
	}
	q, err := New([]Provider{failing{}, s("a", "7")}, nil).Quote(ctx, "BTC/USD")
	if err != nil || FormatRate(q.Rate) != "7" {
		t.Errorf("failing source not skipped: %v %v", q, err)
	}
}

type namedStatic struct {
	name  string
	rates map[string]string
}

func (n namedStatic) Name() string { return n.name }
func (n namedStatic) Rates(ctx context.Context) (map[string]*big.Rat, error) {
	return Static(n.rates).Rates(ctx)
}

type failing struct{}

func (failing) Name() string                                       { return "failing" }
func (failing) Rates(context.Context) (map[string]*big.Rat, error) { return nil, errors.New("down") }

func TestFormatAndParse(t *testing.T) {
	for in, want := range map[string]string{
		"15000000": "15000000", "1/150": "0.00666667", "0.000000005": "0.00000001", "0.000000004999": "0",
		"2.50": "2.5", "1/3": "0.33333333", "123456789.123456785": "123456789.12345679",
	} {
		if got := FormatRate(rat(in)); got != want {
			t.Errorf("FormatRate(%s) = %s, want %s", in, got, want)
		}
	}
	if r, err := ParseRate("150.25"); err != nil || r.Cmp(rat("601/4")) != 0 {
		t.Errorf("parse %v %v", r, err)
	}
	for _, bad := range []string{"", "0", "-1", "1/2", "1e5", "abc"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) accepted", bad)
		}
	}
}

type fakeCaller struct {
	answers map[common.Address]*big.Int
}

func (f fakeCaller) Call(_ context.Context, c common.Address, _ abi.ABI, method string, _ ...any) ([]any, error) {
	switch method {
	case "decimals":
		return []any{uint8(8)}, nil
	case "latestRoundData":
		a, ok := f.answers[c]
		if !ok {
			return nil, errors.New("no feed")
		}
		return []any{big.NewInt(1), a, big.NewInt(0), big.NewInt(0), big.NewInt(1)}, nil
	}
	return nil, errors.New("unknown method")
}

func TestChainlink(t *testing.T) {
	btc, jpy, usdc := common.HexToAddress("0x01"), common.HexToAddress("0x02"), common.HexToAddress("0x03")
	caller := fakeCaller{answers: map[common.Address]*big.Int{
		btc: big.NewInt(100000_00000000), jpy: big.NewInt(666667), usdc: big.NewInt(1_00000000),
	}}
	feeds := map[string]common.Address{"BTC/USD": btc, "JPY/USD": jpy, "USDC/USD": usdc}
	ps, err := FromConfig([]config.FXSource{{Type: "chainlink"}}, nil, nil, caller, feeds)
	if err != nil {
		t.Fatal(err)
	}
	s := New(ps, nil)
	q, err := s.Quote(context.Background(), "BTC/USD")
	if err != nil || FormatRate(q.Rate) != "100000" {
		t.Fatalf("BTC/USD %v %v", q, err)
	}
	q, err = s.Quote(context.Background(), "USDC/JPY")
	if err != nil || FormatRate(q.Rate) != "149.999925" {
		t.Fatalf("USDC/JPY %v %v", FormatRate(q.Rate), err)
	}
	if _, err := FromConfig([]config.FXSource{{Type: "chainlink"}}, nil, nil, nil, feeds); err == nil {
		t.Fatal("chainlink without caller accepted")
	}
	if _, err := FromConfig([]config.FXSource{{Type: "nope"}}, nil, nil, nil, nil); err == nil {
		t.Fatal("unknown type accepted")
	}
}
