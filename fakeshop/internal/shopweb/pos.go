package shopweb

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

// The store terminal of cash-store.test. Someone standing at the counter
// (in the lab: shopper-bot) rings up the items, enters where to ship them
// and presses "現金を受け取った" once the cash is handed over.

func (s *Server) posView(r *http.Request) view {
	return s.newView(r, texts[s.shop.Lang]["posTitle"])
}

func (s *Server) showPOS(w http.ResponseWriter, r *http.Request) {
	render(w, http.StatusOK, "pos", s.posView(r))
}

// submitPOS handles both buttons: action=quote recalculates the total,
// action=sale records the cash sale and prints the receipt.
func (s *Server) submitPOS(w http.ResponseWriter, r *http.Request) {
	v := s.posView(r)
	v.Address = addressFromForm(r)
	var lines []orders.Line
	for _, p := range s.shop.Products {
		n, _ := strconv.Atoi(r.FormValue("qty." + p.SKU))
		if n > 0 {
			v.Qty[p.SKU] = n
			lines = append(lines, orders.Line{SKU: p.SKU, Qty: n})
		}
	}

	items, subtotal, err := orders.PriceLines(s.shop, lines)
	if err != nil {
		v.Error = err.Error()
		render(w, http.StatusUnprocessableEntity, "pos", v)
		return
	}
	v.Items, v.Subtotal, v.Total, v.Quoted = items, subtotal, subtotal.Add(s.shop.Shipping), true
	if r.FormValue("action") != "sale" {
		render(w, http.StatusOK, "pos", v)
		return
	}

	o, err := s.orders.Create(s.shop, lines, v.Address, orders.PayCash)
	if err != nil {
		v.Error = err.Error()
		render(w, http.StatusUnprocessableEntity, "pos", v)
		return
	}
	rv := s.newView(r, texts[s.shop.Lang]["receipt"])
	rv.Order, rv.Items = o, o.Items
	render(w, http.StatusOK, "receipt", rv)
}

// apiCashSale is the JSON form of the terminal:
// {"items":[{"sku","qty"}],"shipping":Address} → order.
func (s *Server) apiCashSale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items    []orders.Line  `json:"items"`
		Shipping orders.Address `json:"shipping"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	o, err := s.orders.Create(s.shop, body.Items, body.Shipping, orders.PayCash)
	if err != nil {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	_, t, _ := s.orders.Get(o.ID)
	httpx.WriteJSON(w, http.StatusCreated, toOrderJSON(o, t))
}
