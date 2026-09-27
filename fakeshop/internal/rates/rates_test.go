package rates

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, s *Store, method, target, body string, admin bool) (int, string) {
	t.Helper()
	h := Handler(s, func(r *http.Request) bool { return r.Header.Get("X-Admin-Token") == "lab" },
		func() time.Time { return time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC) })
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if admin {
		req.Header.Set("X-Admin-Token", "lab")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestFrankfurter(t *testing.T) {
	s := NewStore()
	code, body := serve(t, s, "GET", "/frankfurter/latest?from=USD&to=JPY", "", false)
	want := `{"amount":1.0,"base":"USD","date":"2026-09-27","rates":{"JPY":150}}`
	if code != 200 || body != want {
		t.Fatalf("got %d %s", code, body)
	}
	_, body = serve(t, s, "GET", "/frankfurter/latest?from=JPY&to=USD", "", false)
	if !strings.Contains(body, `"USD":0.0066666667`) {
		t.Fatalf("inverse: %s", body)
	}
	if code, _ := serve(t, s, "GET", "/frankfurter/latest?from=USD&to=EUR", "", false); code != 404 {
		t.Fatalf("unknown currency: %d", code)
	}
}

func TestCoinGecko(t *testing.T) {
	s := NewStore()
	_, body := serve(t, s, "GET", "/coingecko/api/v3/simple/price?ids=bitcoin,usd-coin&vs_currencies=usd,jpy", "", false)
	var got map[string]map[string]json.Number
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["bitcoin"]["usd"] != "100000" || got["bitcoin"]["jpy"] != "15000000" ||
		got["usd-coin"]["usd"] != "1" || got["usd-coin"]["jpy"] != "150" {
		t.Fatalf("got %s", body)
	}
	_, body = serve(t, s, "GET", "/coingecko/api/v3/simple/price?ids=dogecoin&vs_currencies=usd", "", false)
	if body != "{}" {
		t.Fatalf("unknown id: %s", body)
	}
}

func TestAdminRates(t *testing.T) {
	s := NewStore()
	if code, _ := serve(t, s, "POST", "/admin/rates", `{"BTC/USD":"90000"}`, false); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	for _, bad := range []string{`{"BTC/EUR":"1"}`, `{"BTC/USD":"-1"}`, `{"BTC/USD":"1e5"}`, `[1]`} {
		if code, _ := serve(t, s, "POST", "/admin/rates", bad, true); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	code, body := serve(t, s, "POST", "/admin/rates", `{"BTC/USD":"90000.5","USD/JPY":"140"}`, true)
	if code != 200 || body != `{"BTC/USD":"90000.5","USD/JPY":"140","USDC/USD":"1"}` {
		t.Fatalf("update: %d %s", code, body)
	}
	_, body = serve(t, s, "GET", "/coingecko/api/v3/simple/price?ids=bitcoin&vs_currencies=jpy", "", false)
	if body != `{"bitcoin":{"jpy":12600070}}` {
		t.Fatalf("derived BTC/JPY: %s", body)
	}
	_, body = serve(t, s, "GET", "/admin/rates", "", false)
	if !strings.Contains(body, `"USD/JPY":"140"`) {
		t.Fatalf("GET: %s", body)
	}
}
