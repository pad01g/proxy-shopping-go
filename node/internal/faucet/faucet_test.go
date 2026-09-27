package faucet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
)

func TestAmounts(t *testing.T) {
	if got := SatsToBTC(123456789); got != "1.23456789" {
		t.Fatal(got)
	}
	if got := SatsToBTC(1000); got != "0.00001000" {
		t.Fatal(got)
	}
	if v, err := decimalUnits("1.5", 18); err != nil || v.String() != "1500000000000000000" {
		t.Fatal(v, err)
	}
	if v, err := decimalUnits("1000", 6); err != nil || v.String() != "1000000000" {
		t.Fatal(v, err)
	}
	for _, bad := range []string{"0.0000001", "-1", "x"} {
		if _, err := decimalUnits(bad, 6); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	var n flexInt
	if json.Unmarshal([]byte(`"12"`), &n) != nil || n != 12 || json.Unmarshal([]byte(`7`), &n) != nil || n != 7 {
		t.Fatal("flexInt")
	}
}

func TestHandlerValidation(t *testing.T) {
	rpc, _ := bitcoinrpc.New("http://127.0.0.1:1")
	f := New(Config{BTC: rpc})
	h := f.Handler()
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/btc", `{"address":"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4","sats":1}`, http.StatusBadRequest},
		{"/btc", `{"address":`, http.StatusBadRequest},
		{"/mine", `{"blocks":0}`, http.StatusBadRequest},
		{"/evm", `{"address":"0x1"}`, http.StatusBadRequest},
		{"/evm", `{"address":"0x0000000000000000000000000000000000000001","eth":"1"}`, http.StatusBadGateway}, // EVM not configured
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), `"error"`) || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s %s: %d %s", c.path, c.body, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/btc", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight %d", rec.Code)
	}
	if _, err := ParseKey("0x" + AnvilKey0); err != nil {
		t.Fatal(err)
	}
}
