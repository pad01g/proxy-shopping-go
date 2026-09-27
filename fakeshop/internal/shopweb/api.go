package shopweb

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/money"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/orders"
)

type productJSON struct {
	SKU   string     `json:"sku"`
	Name  string     `json:"name"`
	Price money.JSON `json:"price"`
}

type itemJSON struct {
	SKU       string     `json:"sku"`
	Name      string     `json:"name"`
	Qty       int        `json:"qty"`
	UnitPrice money.JSON `json:"unit_price"`
	Subtotal  money.JSON `json:"subtotal"`
}

// orderJSON is the body of GET /api/orders/{id}.
type orderJSON struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Items       []itemJSON      `json:"items"`
	Subtotal    money.JSON      `json:"subtotal"`
	ShippingFee money.JSON      `json:"shipping_fee"`
	Total       money.JSON      `json:"total"`
	Shipping    orders.Address  `json:"shipping"`
	Tracking    orders.Tracking `json:"tracking"`
	Payment     string          `json:"payment"`
	ReceiptNo   string          `json:"receipt_no,omitempty"`
	CreatedAt   int64           `json:"created_at"`
}

func toOrderJSON(o orders.Order, t orders.Tracking) orderJSON {
	items := make([]itemJSON, len(o.Items))
	for i, it := range o.Items {
		items[i] = itemJSON{SKU: it.SKU, Name: it.Name, Qty: it.Qty, UnitPrice: it.UnitPrice.JSON(), Subtotal: it.Subtotal().JSON()}
	}
	return orderJSON{
		ID: o.ID, Status: o.Status, Items: items,
		Subtotal: o.Subtotal.JSON(), ShippingFee: o.Shipping.JSON(), Total: o.Total.JSON(),
		Shipping: o.Address, Tracking: t, Payment: o.Payment, ReceiptNo: o.ReceiptNo,
		CreatedAt: o.CreatedAt.Unix(),
	}
}

func (s *Server) apiProducts(w http.ResponseWriter, r *http.Request) {
	ps := make([]productJSON, len(s.shop.Products))
	for i, p := range s.shop.Products {
		ps[i] = productJSON{SKU: p.SKU, Name: p.Name, Price: p.Price.JSON()}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"shop":     s.shop.Host,
		"currency": s.shop.Currency,
		"shipping": s.shop.Shipping.JSON(),
		"products": ps,
	})
}

// lookup returns the order if it belongs to this shop.
func (s *Server) lookup(id string) (orders.Order, orders.Tracking, bool) {
	o, t, err := s.orders.Get(id)
	if err != nil || o.Shop != s.shop.Host {
		return orders.Order{}, orders.Tracking{}, false
	}
	return o, t, true
}

func (s *Server) apiOrder(w http.ResponseWriter, r *http.Request) {
	o, t, ok := s.lookup(r.PathValue("id"))
	if !ok {
		httpx.Error(w, http.StatusNotFound, "order not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toOrderJSON(o, t))
}

// adminAdvance forces the shipment status: {"to": "shipped"}.
func (s *Server) adminAdvance(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		httpx.Error(w, http.StatusUnauthorized, "bad admin token")
		return
	}
	var body struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !orders.ValidStatus(body.To) {
		httpx.Error(w, http.StatusBadRequest, `body must be {"to": "processing|shipped|delivered|failed"}`)
		return
	}
	id := r.PathValue("id")
	if _, _, ok := s.lookup(id); !ok {
		httpx.Error(w, http.StatusNotFound, "order not found")
		return
	}
	t, err := s.orders.Advance(id, body.To)
	switch {
	case errors.Is(err, orders.ErrNotPaid), errors.Is(err, orders.ErrBackwards):
		httpx.Error(w, http.StatusConflict, err.Error())
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, err.Error())
	default:
		httpx.WriteJSON(w, http.StatusOK, t)
	}
}
