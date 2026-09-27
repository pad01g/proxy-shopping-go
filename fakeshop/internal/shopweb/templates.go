package shopweb

import (
	"bytes"
	"embed"
	"html/template"
	"log"
	"net/http"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/money"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

//go:embed templates/*.html
var templateFS embed.FS

var pages = parsePages("catalog", "product", "cart", "checkout", "complete", "order", "cheappay", "pos", "receipt")

func parsePages(names ...string) map[string]*template.Template {
	funcs := template.FuncMap{
		// money packs an element id with an amount for the "money" partial.
		"money": func(id string, m money.Money) any {
			return struct {
				ID string
				M  money.Money
			}{id, m}
		},
	}
	out := map[string]*template.Template{}
	for _, n := range names {
		out[n] = template.Must(template.New(n).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+n+".html"))
	}
	return out
}

// view carries everything any page template may need.
type view struct {
	Shop      *catalog.Shop
	T         map[string]string
	Title     string
	CartCount int
	Error     string

	Product  catalog.Product
	Items    []orders.Item
	Subtotal money.Money
	Total    money.Money
	Order    orders.Order
	Tracking orders.Tracking
	Address  orders.Address

	Qty    map[string]int // POS form echo
	Quoted bool           // POS total has been calculated
}

func (s *Server) newView(r *http.Request, title string) view {
	v := view{Shop: s.shop, T: texts[s.shop.Lang], Title: title, Qty: map[string]int{}}
	for _, l := range readCart(r) {
		v.CartCount += l.Qty
	}
	return v
}

func render(w http.ResponseWriter, status int, page string, v view) {
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		log.Printf("shopweb: render %s: %v", page, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}
