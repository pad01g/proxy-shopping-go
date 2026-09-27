package shop

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
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
	trusting.AllowPrivate = true // the test servers listen on 127.0.0.1
	strict := NewInspector(&tls.Config{RootCAs: x509.NewCertPool()})
	strict.AllowPrivate = true
	ctx := context.Background()
	p := Policy{Allowlist: []string{"127.0.0.1"}, KnownGateways: []string{"cardgw.test"}, Threshold: 70}

	info, err := trusting.Inspect(ctx, tlsSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Score(info); s != 100 {
		t.Fatalf("score %d %+v", s, info)
	}
	// a certificate that does not verify is never read insecurely
	if info, err := strict.Inspect(ctx, tlsSrv.URL); err == nil {
		t.Fatalf("unverified shop read: %+v", info)
	}
	if _, err := strict.Catalog(ctx, tlsSrv.URL); err == nil {
		t.Fatal("unverified catalog read")
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

	c, err := trusting.Catalog(ctx, tlsSrv.URL+"/some/page")
	if err != nil {
		t.Fatal(err)
	}
	if pr, ok := c.Product("A-100"); !ok || pr.Price.Amount != "3200" || c.Shipping.Amount != "800" {
		t.Fatalf("catalog %+v", c)
	}
	// an HTTPS page that redirects to plain HTTP scores no TLS points, and a redirect to another host loses the
	// allowlist points of the shop's host
	redir := http.NewServeMux()
	redir.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL+"/", http.StatusFound) })
	redir.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(plain.URL, "127.0.0.1", "localhost", 1)+"/", http.StatusFound)
	})
	rsrv := httptest.NewUnstartedServer(redir)
	rsrv.TLS = tlsSrv.TLS
	rsrv.StartTLS()
	defer rsrv.Close()
	info, err = trusting.Inspect(ctx, rsrv.URL+"/down")
	if err != nil || info.CertOK {
		t.Fatalf("https → http redirect: %+v %v", info, err)
	}
	if s, _ := p.Score(info); s != 80 {
		t.Fatalf("score after a downgrade %d", s)
	}
	info, err = trusting.Inspect(ctx, rsrv.URL+"/away")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Score(info); s != 30 || info.FinalHost != "localhost" {
		t.Fatalf("redirect to another host: %d %+v", s, info)
	}
	if _, err := ParseURL("ftp://x"); err == nil || !strings.Contains(err.Error(), "bad shop url") {
		t.Fatal("ftp accepted")
	}
}

func TestPrivateShopsRefused(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(page)) }))
	defer srv.Close()
	in := NewInspector(&tls.Config{})
	for _, u := range []string{srv.URL, "http://localhost:" + srv.URL[strings.LastIndex(srv.URL, ":")+1:]} {
		if _, err := in.Inspect(context.Background(), u); !errors.Is(err, ErrPrivateAddress) {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if _, err := in.Catalog(context.Background(), srv.URL); !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("catalog: %v", err)
	}
	if hits != 0 {
		t.Fatalf("private server reached %d times", hits)
	}
	for addr, ok := range map[string]bool{
		"8.8.8.8:443": true, "[2001:4860:4860::8888]:443": true,
		"127.0.0.1:80": false, "10.1.2.3:80": false, "172.20.0.2:443": false, "192.168.1.1:80": false,
		"169.254.169.254:80": false, "100.64.0.1:80": false, "0.0.0.0:80": false, "[::1]:80": false,
		"[fe80::1]:80": false, "[fd00::1]:80": false, "[::ffff:127.0.0.1]:80": false,
		// IPv6 prefixes that reach any IPv4 address: NAT64 and 6to4
		"[64:ff9b::7f00:1]:80": false, "[64:ff9b:1::a00:1]:80": false, "[2002:7f00:1::1]:80": false,
	} {
		if err := checkPublic(addr); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if _, err := ParseURL("https://user:pw@shop.test/"); err == nil {
		t.Error("url with credentials accepted")
	}
}
