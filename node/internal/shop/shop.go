// Package shop inspects a shop before the shopper quotes: its page (for the risk rules of spec §8) and its
// product catalog (GET /api/products of the lab shops).
package shop

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
)

// Info is what the shop page tells about the shop.
type Info struct {
	URL      string `json:"url"`
	Host     string `json:"host"`
	HTTPS    bool   `json:"https"`
	CertOK   bool   `json:"cert_ok"` // HTTPS and the certificate verified
	Gateway  string `json:"gateway,omitempty"`
	CashOnly bool   `json:"cash_only"`
	Region   string `json:"region,omitempty"`
}

// Inspector fetches shop pages and catalogs. Certificates are always verified: a shop whose certificate does not
// verify is not read at all, and a plain http:// shop simply scores no TLS points. Unless AllowPrivate is set,
// connections to loopback, link-local and private addresses are refused (checked on the resolved address of
// every connection, redirects included), so that a request cannot make the shopper probe its own network.
type Inspector struct {
	HTTP         *http.Client
	AllowPrivate bool
}

// NewInspector builds an inspector around the TLS configuration of the node.
func NewInspector(tc *tls.Config) *Inspector {
	in := &Inspector{}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if in.AllowPrivate {
			return nil
		}
		return checkPublic(address)
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	tr.Proxy = nil // the address check must see the shop, not a proxy
	tr.DialContext = dialer.DialContext
	in.HTTP = &http.Client{Transport: tr, Timeout: 15 * time.Second}
	return in
}

// ErrPrivateAddress is returned for shops on loopback, link-local or private addresses.
var ErrPrivateAddress = errors.New("shop address is not public")

func checkPublic(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, host)
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, ip)
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// ParseURL checks a shop URL.
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("bad shop url %q", raw)
	}
	return u, nil
}

// Inspect fetches the shop page and reads the ps-* meta tags.
func (in *Inspector) Inspect(ctx context.Context, raw string) (*Info, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	info := &Info{URL: raw, Host: strings.ToLower(u.Hostname()), HTTPS: u.Scheme == "https"}
	body, err := get(ctx, in.HTTP, raw)
	if err != nil {
		return nil, err
	}
	info.CertOK = info.HTTPS
	meta := metaTags(body)
	info.Gateway = strings.ToLower(meta["ps-payment-gateway"])
	info.CashOnly = meta["ps-payment"] == "cash-only"
	info.Region = meta["ps-region"]
	return info, nil
}

func get(ctx context.Context, hc *http.Client, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: HTTP %d", raw, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}

// metaTags returns name → content of the <meta> tags of a page.
func metaTags(page []byte) map[string]string {
	out := map[string]string{}
	z := html.NewTokenizer(strings.NewReader(string(page)))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data != "meta" {
				continue
			}
			var name, content string
			for _, a := range t.Attr {
				switch strings.ToLower(a.Key) {
				case "name":
					name = strings.ToLower(a.Val)
				case "content":
					content = a.Val
				}
			}
			if name != "" {
				out[name] = content
			}
		}
	}
}

// Policy is the shopper's risk policy.
type Policy struct {
	Allowlist     []string
	KnownGateways []string
	Threshold     int
}

// Score implements the table of §8 and explains the points.
func (p Policy) Score(info *Info) (int, []string) {
	score := 0
	var why []string
	if slices.Contains(p.Allowlist, info.Host) {
		score += 50
		why = append(why, "allowlisted host +50")
	}
	if info.CertOK {
		score += 20
		why = append(why, "verified HTTPS +20")
	}
	if info.Gateway != "" && slices.Contains(p.KnownGateways, info.Gateway) {
		score += 30
		why = append(why, "known payment gateway "+info.Gateway+" +30")
	}
	return score, why
}

// Product is one item of a catalog.
type Product struct {
	SKU   string `json:"sku"`
	Name  string `json:"name"`
	Price Money  `json:"price"`
}

// Money is an amount with its currency.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Catalog is GET /api/products of a shop.
type Catalog struct {
	Shop     string    `json:"shop"`
	Currency string    `json:"currency"`
	Shipping Money     `json:"shipping"`
	Products []Product `json:"products"`
}

// Product finds a SKU.
func (c *Catalog) Product(sku string) (Product, bool) {
	for _, p := range c.Products {
		if p.SKU == sku {
			return p, true
		}
	}
	return Product{}, false
}

// Catalog reads the product list of the shop at raw (its origin + /api/products).
func (in *Inspector) Catalog(ctx context.Context, raw string) (*Catalog, error) {
	u, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	body, err := get(ctx, in.HTTP, u.Scheme+"://"+u.Host+"/api/products")
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var c Catalog
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if c.Currency == "" {
		return nil, errors.New("catalog without currency")
	}
	return &c, nil
}
