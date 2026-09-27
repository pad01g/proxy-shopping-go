package shopweb

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

// The cart lives in a cookie as a query string: "A-100=1&A-200=2".
// Cookies are per host, so each shop has its own cart.
const cartCookie = "ps_cart"

func readCart(r *http.Request) []orders.Line {
	c, err := r.Cookie(cartCookie)
	if err != nil {
		return nil
	}
	q, err := url.ParseQuery(c.Value)
	if err != nil {
		return nil
	}
	var lines []orders.Line
	for sku := range q {
		if n, err := strconv.Atoi(q.Get(sku)); err == nil && n > 0 {
			lines = append(lines, orders.Line{SKU: sku, Qty: n})
		}
	}
	return lines
}

// sortedCart returns the cart in catalog order, dropping unknown SKUs.
func sortedCart(shop *catalog.Shop, r *http.Request) []orders.Line {
	qty := map[string]int{}
	for _, l := range readCart(r) {
		qty[l.SKU] = l.Qty
	}
	var out []orders.Line
	for _, p := range shop.Products {
		if n := qty[p.SKU]; n > 0 {
			out = append(out, orders.Line{SKU: p.SKU, Qty: n})
		}
	}
	return out
}

func writeCart(w http.ResponseWriter, lines []orders.Line) {
	q := url.Values{}
	for _, l := range lines {
		q.Set(l.SKU, strconv.Itoa(l.Qty))
	}
	c := &http.Cookie{Name: cartCookie, Value: q.Encode(), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if len(lines) == 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
