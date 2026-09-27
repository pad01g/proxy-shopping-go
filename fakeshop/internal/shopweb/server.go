// Package shopweb serves one fictional shop: HTML storefront, JSON API and
// admin hooks. Which pages exist depends on how the shop takes payment.
package shopweb

import (
	"net/http"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/cardgw"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

type Config struct {
	Shop    *catalog.Shop
	Orders  *orders.Store
	Gateway *cardgw.Gateway
	AdminOK func(*http.Request) bool
	// Scheme forces the scheme of absolute URLs (redirects to cardgw and
	// back). Empty means "whatever the browser used".
	Scheme string
}

type Server struct {
	shop    *catalog.Shop
	orders  *orders.Store
	gw      *cardgw.Gateway
	adminOK func(*http.Request) bool
	scheme  string
	mux     *http.ServeMux
}

func New(c Config) *Server {
	s := &Server{shop: c.Shop, orders: c.Orders, gw: c.Gateway, adminOK: c.AdminOK, scheme: c.Scheme, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /{$}", s.showCatalog)
	s.mux.HandleFunc("GET /products/{sku}", s.showProduct)
	s.mux.HandleFunc("GET /orders/{id}", s.showOrder)
	s.mux.HandleFunc("GET /.well-known/ps-shop.json", s.wellKnown)
	s.mux.HandleFunc("GET /api/products", s.apiProducts)
	s.mux.HandleFunc("GET /api/orders/{id}", s.apiOrder)
	s.mux.HandleFunc("POST /admin/orders/{id}/advance", s.adminAdvance)

	switch {
	case c.Shop.Payment == catalog.PaymentCashOnly:
		s.mux.HandleFunc("GET /pos", s.showPOS)
		s.mux.HandleFunc("POST /pos", s.submitPOS)
		s.mux.HandleFunc("POST /pos/api/cash-sale", s.apiCashSale)
	default:
		s.mux.HandleFunc("GET /cart", s.showCart)
		s.mux.HandleFunc("POST /cart/add", s.addToCart)
		s.mux.HandleFunc("POST /cart/clear", s.clearCart)
		s.mux.HandleFunc("GET /checkout", s.showCheckout)
		s.mux.HandleFunc("POST /checkout", s.placeOrder)
		if c.Shop.SelfHostedPay {
			s.mux.HandleFunc("GET /cheap-pay", s.showCheapPay)
			s.mux.HandleFunc("POST /cheap-pay", s.submitCheapPay)
		} else {
			s.mux.HandleFunc("GET /checkout/complete", s.completeCheckout)
		}
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// absURL builds a URL on another virtual host of this binary.
func (s *Server) absURL(r *http.Request, host, path string) string {
	return httpx.Scheme(r, s.scheme) + "://" + host + path
}

func (s *Server) wellKnown(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"host":     s.shop.Host,
		"currency": s.shop.Currency,
		"payment":  string(s.shop.Payment),
		"gateway":  s.shop.Gateway,
		"region":   s.shop.Region,
	})
}
