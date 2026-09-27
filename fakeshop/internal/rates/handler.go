package rates

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
)

// fiat are the currencies the Frankfurter imitation knows.
var fiat = map[string]bool{"USD": true, "JPY": true}

// coingeckoIDs maps CoinGecko coin ids to our asset names.
var coingeckoIDs = map[string]string{"bitcoin": "BTC", "usd-coin": "USDC"}

// Handler serves rates.test. adminOK authorizes POST /admin/rates.
func Handler(s *Store, adminOK func(*http.Request) bool, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /frankfurter/latest", func(w http.ResponseWriter, r *http.Request) {
		frankfurter(s, now, w, r)
	})
	mux.HandleFunc("GET /coingecko/api/v3/simple/price", func(w http.ResponseWriter, r *http.Request) {
		coingecko(s, w, r)
	})
	mux.HandleFunc("GET /admin/rates", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, s.Snapshot())
	})
	mux.HandleFunc("POST /admin/rates", func(w http.ResponseWriter, r *http.Request) {
		if !adminOK(r) {
			httpx.Error(w, http.StatusUnauthorized, "bad admin token")
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.Error(w, http.StatusBadRequest, `body must be {"PAIR": "decimal"}`)
			return
		}
		if err := s.Set(body); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, s.Snapshot())
	})
	return mux
}

// frankfurter answers like https://api.frankfurter.app/latest:
// {"amount":1.0,"base":"USD","date":"2026-09-27","rates":{"JPY":150.0}}
func frankfurter(s *Store, now func() time.Time, w http.ResponseWriter, r *http.Request) {
	from := strings.ToUpper(r.URL.Query().Get("from"))
	if from == "" {
		from = "USD" // the real API defaults to EUR, which the lab does not have
	}
	if !fiat[from] {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	var to []string
	if t := r.URL.Query().Get("to"); t != "" {
		to = strings.Split(strings.ToUpper(t), ",")
	} else {
		for c := range fiat {
			if c != from {
				to = append(to, c)
			}
		}
	}
	out := map[string]json.Number{}
	for _, c := range to {
		if !fiat[c] {
			httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
			return
		}
		if c == from {
			continue // the real API leaves the base out
		}
		rate, _ := s.Rate(from, c)
		out[c] = json.Number(decimal(rate))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"amount": json.Number("1.0"),
		"base":   from,
		"date":   now().UTC().Format("2006-01-02"),
		"rates":  out,
	})
}

// coingecko answers like /api/v3/simple/price:
// {"bitcoin":{"usd":100000,"jpy":15000000},"usd-coin":{"usd":1,"jpy":150}}
// Unknown ids and currencies are left out, as the real API does.
func coingecko(s *Store, w http.ResponseWriter, r *http.Request) {
	ids := splitLower(r.URL.Query().Get("ids"))
	vs := splitLower(r.URL.Query().Get("vs_currencies"))
	if len(ids) == 0 || len(vs) == 0 {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing parameter ids or vs_currencies"})
		return
	}
	out := map[string]map[string]json.Number{}
	for _, id := range ids {
		asset, ok := coingeckoIDs[id]
		if !ok {
			continue
		}
		prices := map[string]json.Number{}
		for _, v := range vs {
			if rate, ok := s.Rate(asset, strings.ToUpper(v)); ok && fiat[strings.ToUpper(v)] {
				prices[v] = json.Number(decimal(rate))
			}
		}
		out[id] = prices
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func splitLower(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
