package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// browser drives the app in-process, keeping one cookie jar per host the
// way a real browser would.
type browser struct {
	t       *testing.T
	app     http.Handler
	cookies map[string][]*http.Cookie
	clock   *fakeClock
}

func newBrowser(t *testing.T) (*browser, *App) {
	clock := &fakeClock{t: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	a := New(Options{AdminToken: "lab", GatewaySecret: []byte("test-secret"), Now: clock.Now})
	return &browser{t: t, app: a, cookies: map[string][]*http.Cookie{}, clock: clock}, a
}

type response struct {
	status   int
	body     string
	location string
}

func (b *browser) do(method, rawURL string, body io.Reader, contentType string, header http.Header) response {
	b.t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		b.t.Fatal(err)
	}
	req := httptest.NewRequest(method, u.RequestURI(), body)
	req.Host = u.Host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	for _, c := range b.cookies[u.Host] {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.app.ServeHTTP(rec, req)
	res := rec.Result()
	for _, c := range res.Cookies() {
		b.setCookie(u.Host, c)
	}
	return response{status: res.StatusCode, body: rec.Body.String(), location: res.Header.Get("Location")}
}

func (b *browser) setCookie(host string, c *http.Cookie) {
	jar := b.cookies[host][:0:0]
	for _, old := range b.cookies[host] {
		if old.Name != c.Name {
			jar = append(jar, old)
		}
	}
	if c.MaxAge >= 0 {
		jar = append(jar, c)
	}
	b.cookies[host] = jar
}

func (b *browser) get(u string) response { return b.do("GET", u, nil, "", nil) }

func (b *browser) postForm(u string, form url.Values) response {
	return b.do("POST", u, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
}

func (b *browser) postJSON(u string, v any, header http.Header) response {
	raw, _ := json.Marshal(v)
	return b.do("POST", u, strings.NewReader(string(raw)), "application/json", header)
}

func (b *browser) getJSON(u string, v any) {
	b.t.Helper()
	res := b.get(u)
	if res.status != http.StatusOK {
		b.t.Fatalf("GET %s: %d %s", u, res.status, res.body)
	}
	if err := json.Unmarshal([]byte(res.body), v); err != nil {
		b.t.Fatalf("GET %s: %v", u, err)
	}
}

var admin = http.Header{"X-Admin-Token": {"lab"}}

// element returns the text of the element with the given id.
func element(t *testing.T, html, id string) string {
	t.Helper()
	m := regexp.MustCompile(`id="` + regexp.QuoteMeta(id) + `"[^>]*>([^<]*)<`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no #%s in page:\n%s", id, html)
	}
	return m[1]
}

func attr(t *testing.T, html, id, name string) string {
	t.Helper()
	m := regexp.MustCompile(`id="` + regexp.QuoteMeta(id) + `"[^>]*\s` + name + `="([^"]*)"`).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no #%s[%s] in page", id, name)
	}
	return m[1]
}

var address = url.Values{
	"name": {"山田太郎"}, "postal_code": {"160-0022"}, "address": {"東京都新宿区新宿3-1-1"}, "phone": {"03-0000-0000"},
}
