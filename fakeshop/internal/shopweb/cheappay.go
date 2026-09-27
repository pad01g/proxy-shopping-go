package shopweb

import (
	"net/http"
	"strings"
)

// The risky shop takes card numbers on its own page and accepts anything.
// It exists so that shoppers have something to refuse (spec §8).

func (s *Server) showCheapPay(w http.ResponseWriter, r *http.Request) {
	o, _, err := s.orders.Get(r.URL.Query().Get("order"))
	if err != nil || o.Shop != s.shop.Host {
		http.NotFound(w, r)
		return
	}
	v := s.newView(r, "Pay")
	v.Order = o
	render(w, http.StatusOK, "cheappay", v)
}

func (s *Server) submitCheapPay(w http.ResponseWriter, r *http.Request) {
	o, _, err := s.orders.Get(r.FormValue("order"))
	if err != nil || o.Shop != s.shop.Host {
		http.NotFound(w, r)
		return
	}
	number := strings.ReplaceAll(r.FormValue("number"), " ", "")
	if len(number) < 4 {
		v := s.newView(r, "Pay")
		v.Order, v.Error = o, "card number required"
		render(w, http.StatusUnprocessableEntity, "cheappay", v)
		return
	}
	o, err = s.orders.MarkPaid(o.ID, "rk_"+number[len(number)-4:])
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.renderComplete(w, r, o)
}
