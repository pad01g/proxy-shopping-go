package shop

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const page = `<!doctype html><html><head><meta charset="utf-8">
<meta name="ps-payment-gateway" content="cardgw.test"><title>x</title></head><body></body></html>`

const cashPage = `<html><head><meta name="ps-payment" content="cash-only"><meta name="ps-region" content="JP-13-13104"></head></html>`

func TestInspectAndScore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(page)) })
	mux.HandleFunc("/cash", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(cashPage)) })
	mux.HandleFunc("/api/products", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"shop":"x","currency":"JPY","shipping":{"amount":"800","currency":"JPY"},"products":[{"sku":"A-100","name":"t","price":{"amount":"3200","currency":"JPY"}}]}`))
	})
	tlsSrv := httptest.NewTLSServer(mux)
	defer tlsSrv.Close()
	plain := httptest.NewServer(mux)
	defer plain.Close()

	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	trusting := NewInspector(&tls.Config{RootCAs: pool})
	strict := NewInspector(&tls.Config{RootCAs: x509.NewCertPool()})
	ctx := context.Background()
	p := Policy{Allowlist: []string{"127.0.0.1"}, KnownGateways: []string{"cardgw.test"}, Threshold: 70}

	info, err := trusting.Inspect(ctx, tlsSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Score(info); s != 100 {
		t.Fatalf("score %d %+v", s, info)
	}
	// an unverifiable certificate still gives the page, without the TLS points
	info, err = strict.Inspect(ctx, tlsSrv.URL)
	if err != nil || info.CertOK {
		t.Fatalf("%+v %v", info, err)
	}
	if s, _ := p.Score(info); s != 80 {
		t.Fatalf("score %d", s)
	}
	info, err = trusting.Inspect(ctx, plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := (Policy{KnownGateways: []string{"other"}}).Score(info); s != 0 {
		t.Fatalf("http, unknown gateway, not allowlisted: %d", s)
	}
	info, err = trusting.Inspect(ctx, plain.URL+"/cash")
	if err != nil || !info.CashOnly || info.Region != "JP-13-13104" {
		t.Fatalf("cash %+v %v", info, err)
	}

	c, err := trusting.Catalog(ctx, tlsSrv.URL+"/some/page", true)
	if err != nil {
		t.Fatal(err)
	}
	if pr, ok := c.Product("A-100"); !ok || pr.Price.Amount != "3200" || c.Shipping.Amount != "800" {
		t.Fatalf("catalog %+v", c)
	}
	if _, err := ParseURL("ftp://x"); err == nil || !strings.Contains(err.Error(), "bad shop url") {
		t.Fatal("ftp accepted")
	}
}
