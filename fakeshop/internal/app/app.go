// Package app wires all fakeshop virtual hosts into one http.Handler.
package app

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/cardgw"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/rates"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/shopweb"
)

type Options struct {
	AdminToken    string
	GatewaySecret []byte
	Scheme        string           // see shopweb.Config.Scheme
	Now           func() time.Time // nil = time.Now
}

type App struct {
	Orders  *orders.Store
	Gateway *cardgw.Gateway
	Rates   *rates.Store
	hosts   map[string]http.Handler
}

func New(o Options) *App {
	if o.Now == nil {
		o.Now = time.Now
	}
	adminOK := httpx.AdminToken(o.AdminToken)
	a := &App{
		Orders:  orders.NewStore(o.Now),
		Gateway: cardgw.New(o.GatewaySecret, o.Now),
		Rates:   rates.NewStore(),
		hosts:   map[string]http.Handler{},
	}
	a.hosts[catalog.GatewayHost] = a.Gateway.Handler()
	a.hosts[catalog.RatesHost] = rates.Handler(a.Rates, adminOK, o.Now)
	for _, shop := range catalog.Shops() {
		a.hosts[shop.Host] = shopweb.New(shopweb.Config{
			Shop: shop, Orders: a.Orders, Gateway: a.Gateway, AdminOK: adminOK, Scheme: o.Scheme,
		})
	}
	return a
}

// ServeHTTP routes by virtual host (see httpx.Host).
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok\n"))
		return
	}
	if h, ok := a.hosts[httpx.Host(r)]; ok {
		h.ServeHTTP(w, r)
		return
	}
	names := make([]string, 0, len(a.hosts))
	for n := range a.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	http.Error(w, "unknown host "+httpx.Host(r)+"; this server knows: "+strings.Join(names, ", "), http.StatusNotFound)
}
