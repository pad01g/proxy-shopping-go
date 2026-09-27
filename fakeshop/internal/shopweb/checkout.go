package shopweb

import (
	"net/http"
	"net/url"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/cardgw"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

func addressFromForm(r *http.Request) orders.Address {
	return orders.Address{
		Name:       r.FormValue("name"),
		PostalCode: r.FormValue("postal_code"),
		Address:    r.FormValue("address"),
		Phone:      r.FormValue("phone"),
	}
}

// checkoutView prices the cart; ok is false when the cart is empty.
func (s *Server) checkoutView(r *http.Request) (view, []orders.Line, bool) {
	v := s.newView(r, texts[s.shop.Lang]["checkout"])
	lines := sortedCart(s.shop, r)
	items, subtotal, err := orders.PriceLines(s.shop, lines)
	if err != nil {
		return v, nil, false
	}
	v.Items, v.Subtotal, v.Total = items, subtotal, subtotal.Add(s.shop.Shipping)
	return v, lines, true
}

func (s *Server) showCheckout(w http.ResponseWriter, r *http.Request) {
	v, _, ok := s.checkoutView(r)
	if !ok {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}
	render(w, http.StatusOK, "checkout", v)
}

// placeOrder creates a pending order from the cart and sends the buyer to
// the payment page: cardgw.test, or the shop's own /cheap-pay.
func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	v, lines, ok := s.checkoutView(r)
	if !ok {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}
	v.Address = addressFromForm(r)
	o, err := s.orders.Create(s.shop, lines, v.Address, orders.PayCard)
	if err != nil {
		v.Error = err.Error()
		render(w, http.StatusUnprocessableEntity, "checkout", v)
		return
	}
	writeCart(w, nil)

	if s.shop.SelfHostedPay {
		http.Redirect(w, r, "/cheap-pay?order="+url.QueryEscape(o.ID), http.StatusSeeOther)
		return
	}
	sess := s.gw.CreateSession(s.shop.Host, o.ID, o.Total, s.absURL(r, s.shop.Host, "/checkout/complete"))
	http.Redirect(w, r, s.absURL(r, catalog.GatewayHost, "/pay/"+sess.ID), http.StatusSeeOther)
}

// completeCheckout is the return URL cardgw sends the buyer to. The query
// carries an HMAC over (session, charge, status) that only cardgw can make.
func (s *Server) completeCheckout(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessID, chargeID, status := q.Get("session"), q.Get("charge"), q.Get("status")
	if !s.gw.VerifyReturn(sessID, chargeID, status, q.Get("sig")) {
		http.Error(w, "invalid payment signature", http.StatusBadRequest)
		return
	}
	sess, ok := s.gw.Session(sessID)
	if !ok || sess.Merchant != s.shop.Host || status != cardgw.ChargeSucceeded || sess.ChargeID != chargeID {
		http.Error(w, "payment not completed", http.StatusPaymentRequired)
		return
	}
	o, err := s.orders.MarkPaid(sess.OrderID, chargeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.renderComplete(w, r, o)
}

func (s *Server) renderComplete(w http.ResponseWriter, r *http.Request, o orders.Order) {
	v := s.newView(r, texts[s.shop.Lang]["thanks"])
	v.Order, v.Items = o, o.Items
	render(w, http.StatusOK, "complete", v)
}
