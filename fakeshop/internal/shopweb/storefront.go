package shopweb

import (
	"net/http"
	"strconv"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

func (s *Server) showCatalog(w http.ResponseWriter, r *http.Request) {
	render(w, http.StatusOK, "catalog", s.newView(r, ""))
}

func (s *Server) showProduct(w http.ResponseWriter, r *http.Request) {
	p, ok := s.shop.Product(r.PathValue("sku"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	v := s.newView(r, p.Name)
	v.Product = p
	render(w, http.StatusOK, "product", v)
}

// showOrder is a human-readable order status page.
func (s *Server) showOrder(w http.ResponseWriter, r *http.Request) {
	o, t, err := s.orders.Get(r.PathValue("id"))
	if err != nil || o.Shop != s.shop.Host {
		http.NotFound(w, r)
		return
	}
	v := s.newView(r, o.ID)
	v.Order, v.Tracking, v.Items = o, t, o.Items
	render(w, http.StatusOK, "order", v)
}

func (s *Server) showCart(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, texts[s.shop.Lang]["cart"])
	if lines := sortedCart(s.shop, r); len(lines) > 0 {
		items, subtotal, err := orders.PriceLines(s.shop, lines)
		if err == nil {
			v.Items, v.Subtotal, v.Total = items, subtotal, subtotal.Add(s.shop.Shipping)
		}
	}
	render(w, http.StatusOK, "cart", v)
}

func (s *Server) addToCart(w http.ResponseWriter, r *http.Request) {
	sku := r.FormValue("sku")
	qty, err := strconv.Atoi(r.FormValue("qty"))
	if _, ok := s.shop.Product(sku); !ok || err != nil || qty <= 0 || qty > 99 {
		http.Error(w, "invalid sku or quantity", http.StatusBadRequest)
		return
	}
	lines := sortedCart(s.shop, r)
	found := false
	for i := range lines {
		if lines[i].SKU == sku {
			lines[i].Qty = min(lines[i].Qty+qty, 99)
			found = true
		}
	}
	if !found {
		lines = append(lines, orders.Line{SKU: sku, Qty: qty})
	}
	writeCart(w, lines)
	http.Redirect(w, r, "/cart", http.StatusSeeOther)
}

func (s *Server) clearCart(w http.ResponseWriter, r *http.Request) {
	writeCart(w, nil)
	http.Redirect(w, r, "/cart", http.StatusSeeOther)
}
