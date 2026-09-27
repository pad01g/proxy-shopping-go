package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type orderResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Items  []struct {
		SKU string `json:"sku"`
		Qty int    `json:"qty"`
	} `json:"items"`
	Total struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"total"`
	Shipping map[string]string `json:"shipping"`
	Tracking struct {
		Status     string `json:"status"`
		Carrier    string `json:"carrier"`
		TrackingNo string `json:"tracking_no"`
		UpdatedAt  int64  `json:"updated_at"`
	} `json:"tracking"`
	Payment   string `json:"payment"`
	ReceiptNo string `json:"receipt_no"`
}

// buyWithCard runs the storefront → cardgw → confirmation flow and returns
// the confirmation page.
func buyWithCard(t *testing.T, b *browser, shop string, lines map[string]int, wantTotal string) string {
	t.Helper()
	base := "http://" + shop
	for sku, n := range lines {
		res := b.postForm(base+"/cart/add", url.Values{"sku": {sku}, "qty": {strconv.Itoa(n)}})
		if res.status != http.StatusSeeOther {
			t.Fatalf("add %s: %d", sku, res.status)
		}
	}
	page := b.get(base + "/checkout").body
	if got := attr(t, page, "checkout-total", "data-amount"); got != wantTotal {
		t.Fatalf("checkout total = %s, want %s", got, wantTotal)
	}

	res := b.postForm(base+"/checkout", address)
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "http://cardgw.test/pay/cs_") {
		t.Fatalf("place order: %d %q", res.status, res.location)
	}
	payURL := res.location
	pay := b.get(payURL).body
	if got := attr(t, pay, "pay-amount", "data-amount"); got != wantTotal {
		t.Fatalf("gateway amount = %s, want %s", got, wantTotal)
	}

	card := url.Values{"number": {"4242 4242 4242 4242"}, "exp": {"12/30"}, "cvc": {"123"}, "name": {"TARO YAMADA"}}
	res = b.postForm(payURL, card)
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, base+"/checkout/complete?") {
		t.Fatalf("pay: %d %q", res.status, res.location)
	}
	done := b.get(res.location)
	if done.status != http.StatusOK {
		t.Fatalf("complete: %d %s", done.status, done.body)
	}
	return done.body
}

func TestCardFlowSafeShop(t *testing.T) {
	b, _ := newBrowser(t)
	page := buyWithCard(t, b, "safe-shop.test", map[string]int{"A-100": 2}, "7200")

	id := element(t, page, "order-id")
	if attr(t, page, "order-total", "data-amount") != "7200" || !strings.HasPrefix(id, "SS-") {
		t.Fatalf("confirmation: id=%s", id)
	}
	var o orderResp
	b.getJSON("http://safe-shop.test/api/orders/"+id, &o)
	if o.Status != "paid" || o.Payment != "card" || o.Total.Amount != "7200" || o.Total.Currency != "JPY" ||
		o.Tracking.Status != "processing" || o.Shipping["postal_code"] != "160-0022" || o.Items[0].Qty != 2 {
		t.Fatalf("order = %+v", o)
	}
	// The cart is emptied once the order is placed.
	if !strings.Contains(b.get("http://safe-shop.test/cart").body, `id="cart-empty"`) {
		t.Error("cart should be empty after checkout")
	}
	// Orders are only visible on their own shop.
	if res := b.get("http://us-shop.test/api/orders/" + id); res.status != http.StatusNotFound {
		t.Errorf("order leaked to another shop: %d", res.status)
	}
}

func TestCardFlowUSShop(t *testing.T) {
	b, _ := newBrowser(t)
	page := buyWithCard(t, b, "us-shop.test", map[string]int{"U-100": 1}, "35.00")
	if got := element(t, page, "order-total"); got != "$35.00" {
		t.Fatalf("order total text = %q", got)
	}
}

func TestDeclinedCard(t *testing.T) {
	b, _ := newBrowser(t)
	b.postForm("http://safe-shop.test/cart/add", url.Values{"sku": {"A-200"}, "qty": {"1"}})
	res := b.postForm("http://safe-shop.test/checkout", address)
	payURL := res.location

	res = b.postForm(payURL, url.Values{"number": {"4000000000000002"}, "exp": {"12/30"}, "cvc": {"123"}, "name": {"X"}})
	if res.status != http.StatusPaymentRequired || attr(t, res.body, "pay-error", "data-reason") != "card_declined" {
		t.Fatalf("decline: %d", res.status)
	}
	var raw map[string]any
	b.getJSON("http://cardgw.test/api/charges/"+attr(t, res.body, "pay-error", "data-charge"), &raw)
	if raw["status"] != "declined" || raw["card_last4"] != "0002" || raw["amount"].(map[string]any)["amount"] != "12800" {
		t.Fatalf("charge = %v", raw)
	}

	// Bad input is rejected without a charge; a retry with a good card works.
	res = b.postForm(payURL, url.Values{"number": {"4242424242424242"}, "exp": {"01/20"}, "cvc": {"123"}, "name": {"X"}})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("expired card: %d", res.status)
	}
	res = b.postForm(payURL, url.Values{"number": {"4242424242424242"}, "exp": {"12/30"}, "cvc": {"123"}, "name": {"X"}})
	if res.status != http.StatusSeeOther {
		t.Fatalf("retry: %d", res.status)
	}
	charge := mustQuery(t, res.location).Get("charge")
	b.getJSON("http://cardgw.test/api/charges/"+charge, &raw)
	if raw["status"] != "succeeded" {
		t.Fatalf("charge = %v", raw)
	}
	// Paying the same session twice is refused.
	if res := b.postForm(payURL, url.Values{"number": {"4242424242424242"}, "exp": {"12/30"}, "cvc": {"123"}, "name": {"X"}}); res.status != http.StatusConflict {
		t.Fatalf("double pay: %d", res.status)
	}
}

func TestForgedReturnIsRejected(t *testing.T) {
	b, _ := newBrowser(t)
	b.postForm("http://safe-shop.test/cart/add", url.Values{"sku": {"A-100"}, "qty": {"1"}})
	payURL := b.postForm("http://safe-shop.test/checkout", address).location
	session := payURL[strings.LastIndex(payURL, "/")+1:]

	q := url.Values{"session": {session}, "charge": {"ch_fake"}, "status": {"succeeded"}, "sig": {strings.Repeat("0", 64)}}
	if res := b.get("http://safe-shop.test/checkout/complete?" + q.Encode()); res.status != http.StatusBadRequest {
		t.Fatalf("forged return: %d", res.status)
	}
}

func TestSchemeFollowsProxy(t *testing.T) {
	b, _ := newBrowser(t)
	b.postForm("http://safe-shop.test/cart/add", url.Values{"sku": {"A-100"}, "qty": {"1"}})
	res := b.do("POST", "http://safe-shop.test/checkout", strings.NewReader(address.Encode()),
		"application/x-www-form-urlencoded", http.Header{"X-Forwarded-Proto": {"https"}})
	if !strings.HasPrefix(res.location, "https://cardgw.test/pay/") {
		t.Fatalf("location = %q", res.location)
	}
}

func TestTrackingProgression(t *testing.T) {
	b, _ := newBrowser(t)
	id := element(t, buyWithCard(t, b, "safe-shop.test", map[string]int{"A-100": 1}, "4000"), "order-id")
	u := "http://safe-shop.test/api/orders/" + id

	var o orderResp
	b.getJSON(u, &o)
	if o.Tracking.Status != "processing" || o.Tracking.TrackingNo != "" {
		t.Fatalf("t=0: %+v", o.Tracking)
	}
	b.clock.Advance(2 * time.Second)
	b.getJSON(u, &o)
	if o.Tracking.Status != "shipped" || o.Tracking.Carrier != "Yamato" || len(o.Tracking.TrackingNo) != 12 {
		t.Fatalf("t=2s: %+v", o.Tracking)
	}
	shippedAt := o.Tracking.UpdatedAt
	b.clock.Advance(2 * time.Second)
	b.getJSON(u, &o)
	if o.Tracking.Status != "shipped" {
		t.Fatalf("t=4s: %+v", o.Tracking)
	}
	b.clock.Advance(time.Second)
	b.getJSON(u, &o)
	if o.Tracking.Status != "delivered" || o.Tracking.UpdatedAt != shippedAt+3 {
		t.Fatalf("t=5s: %+v", o.Tracking)
	}
}

func TestFailSKU(t *testing.T) {
	b, _ := newBrowser(t)
	id := element(t, buyWithCard(t, b, "safe-shop.test", map[string]int{"FAIL-100": 1}, "2800"), "order-id")
	u := "http://safe-shop.test/api/orders/" + id
	var o orderResp
	b.clock.Advance(2 * time.Second)
	b.getJSON(u, &o)
	if o.Tracking.Status != "shipped" {
		t.Fatalf("t=2s: %+v", o.Tracking)
	}
	b.clock.Advance(3 * time.Second)
	b.getJSON(u, &o)
	if o.Tracking.Status != "failed" {
		t.Fatalf("t=5s: %+v", o.Tracking)
	}
}

func TestAdminAdvance(t *testing.T) {
	b, _ := newBrowser(t)
	id := element(t, buyWithCard(t, b, "safe-shop.test", map[string]int{"A-100": 1}, "4000"), "order-id")
	u := "http://safe-shop.test/admin/orders/" + id + "/advance"

	if res := b.postJSON(u, map[string]string{"to": "delivered"}, nil); res.status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.status)
	}
	if res := b.postJSON(u, map[string]string{"to": "lost"}, admin); res.status != http.StatusBadRequest {
		t.Fatalf("bad status: %d", res.status)
	}
	res := b.postJSON(u, map[string]string{"to": "shipped"}, admin)
	var tr struct{ Status string }
	json.Unmarshal([]byte(res.body), &tr)
	if res.status != http.StatusOK || tr.Status != "shipped" {
		t.Fatalf("advance: %d %s", res.status, res.body)
	}
	if res := b.postJSON(u, map[string]string{"to": "processing"}, admin); res.status != http.StatusConflict {
		t.Fatalf("backwards: %d", res.status)
	}
	b.postJSON(u, map[string]string{"to": "failed"}, admin)
	// The automatic schedule does not overwrite a terminal manual state.
	b.clock.Advance(10 * time.Second)
	var o orderResp
	b.getJSON("http://safe-shop.test/api/orders/"+id, &o)
	if o.Tracking.Status != "failed" {
		t.Fatalf("after schedule: %+v", o.Tracking)
	}
}

func TestCashFlowPOS(t *testing.T) {
	b, _ := newBrowser(t)
	form := url.Values{"qty.C-100": {"2"}, "action": {"quote"}}
	for k, v := range address {
		form[k] = v
	}
	quote := b.postForm("http://cash-store.test/pos", form)
	if quote.status != http.StatusOK || attr(t, quote.body, "pos-total", "data-amount") != "4000" {
		t.Fatalf("quote: %d", quote.status)
	}
	if strings.Contains(quote.body, `id="receipt-no"`) {
		t.Fatal("quote must not record a sale")
	}

	form.Set("action", "sale")
	receipt := b.postForm("http://cash-store.test/pos", form)
	if receipt.status != http.StatusOK {
		t.Fatalf("sale: %d %s", receipt.status, receipt.body)
	}
	id := element(t, receipt.body, "order-id")
	if !strings.HasPrefix(element(t, receipt.body, "receipt-no"), "R20260927-") ||
		attr(t, receipt.body, "order-total", "data-amount") != "4000" {
		t.Fatal("receipt fields")
	}
	var o orderResp
	b.getJSON("http://cash-store.test/api/orders/"+id, &o)
	if o.Payment != "cash" || o.Status != "paid" || o.ReceiptNo == "" || o.Shipping["name"] != "山田太郎" {
		t.Fatalf("order = %+v", o)
	}
	b.clock.Advance(5 * time.Second)
	b.getJSON("http://cash-store.test/api/orders/"+id, &o)
	if o.Tracking.Status != "delivered" {
		t.Fatalf("tracking = %+v", o.Tracking)
	}
}

func TestCashSaleAPI(t *testing.T) {
	b, _ := newBrowser(t)
	body := map[string]any{
		"items":    []map[string]any{{"sku": "C-100", "qty": 1}},
		"shipping": map[string]string{"name": "A", "postal_code": "1", "address": "B", "phone": "2"},
	}
	res := b.postJSON("http://cash-store.test/pos/api/cash-sale", body, nil)
	var o orderResp
	json.Unmarshal([]byte(res.body), &o)
	if res.status != http.StatusCreated || o.Total.Amount != "2500" || o.Payment != "cash" {
		t.Fatalf("cash sale: %d %s", res.status, res.body)
	}
	body["shipping"] = map[string]string{"name": "A"}
	if res := b.postJSON("http://cash-store.test/pos/api/cash-sale", body, nil); res.status != http.StatusUnprocessableEntity {
		t.Fatalf("missing address: %d", res.status)
	}
}

func TestCashStoreHasNoOnlineCheckout(t *testing.T) {
	b, _ := newBrowser(t)
	if res := b.get("http://cash-store.test/checkout"); res.status != http.StatusNotFound {
		t.Fatalf("checkout: %d", res.status)
	}
	if !strings.Contains(b.get("http://cash-store.test/").body, "店頭でのみ現金払い") {
		t.Fatal("catalog should say cash only")
	}
}

func TestRiskyShopSelfHostedPay(t *testing.T) {
	b, _ := newBrowser(t)
	b.postForm("http://risky-shop.test/cart/add", url.Values{"sku": {"R-100"}, "qty": {"1"}})
	res := b.postForm("http://risky-shop.test/checkout", address)
	if res.status != http.StatusSeeOther || !strings.HasPrefix(res.location, "/cheap-pay?order=RK-") {
		t.Fatalf("checkout: %d %q", res.status, res.location)
	}
	order := mustQuery(t, "http://x"+res.location).Get("order")
	done := b.postForm("http://risky-shop.test/cheap-pay", url.Values{"order": {order}, "number": {"1111222233334444"}, "exp": {"x"}, "cvc": {"x"}, "name": {"x"}})
	if done.status != http.StatusOK || element(t, done.body, "order-id") != order ||
		attr(t, done.body, "order-total", "data-amount") != "9.99" {
		t.Fatalf("cheap pay: %d", done.status)
	}
}

func TestMetaTagsAndWellKnown(t *testing.T) {
	b, _ := newBrowser(t)
	cases := []struct {
		host, meta, payment, gateway, region string
	}{
		{"safe-shop.test", `<meta name="ps-payment-gateway" content="cardgw.test">`, "card", "cardgw.test", "JP-13"},
		{"us-shop.test", `<meta name="ps-payment-gateway" content="cardgw.test">`, "card", "cardgw.test", "US"},
		{"risky-shop.test", `<meta name="ps-payment-gateway" content="risky-shop.test">`, "card", "risky-shop.test", "US"},
		{"cash-store.test", `<meta name="ps-payment" content="cash-only">`, "cash-only", "", "JP-13-13104"},
	}
	for _, c := range cases {
		for _, path := range []string{"/", "/products/" + firstSKU(c.host)} {
			if page := b.get("http://" + c.host + path).body; !strings.Contains(page, c.meta) {
				t.Errorf("%s%s: missing %s", c.host, path, c.meta)
			}
		}
		var wk map[string]string
		b.getJSON("http://"+c.host+"/.well-known/ps-shop.json", &wk)
		if wk["host"] != c.host || wk["payment"] != c.payment || wk["gateway"] != c.gateway || wk["region"] != c.region {
			t.Errorf("%s well-known = %v", c.host, wk)
		}
	}
	if page := b.get("http://cash-store.test/").body; !strings.Contains(page, `<meta name="ps-region" content="JP-13-13104">`) {
		t.Error("cash-store: missing ps-region")
	}
}

func firstSKU(host string) string {
	return map[string]string{"safe-shop.test": "A-100", "us-shop.test": "U-100", "risky-shop.test": "R-100", "cash-store.test": "C-100"}[host]
}

func TestProductsAPI(t *testing.T) {
	b, _ := newBrowser(t)
	var p struct {
		Currency string
		Shipping struct{ Amount string }
		Products []struct {
			SKU   string
			Price struct{ Amount, Currency string }
		}
	}
	b.getJSON("http://safe-shop.test/api/products", &p)
	if p.Currency != "JPY" || p.Shipping.Amount != "800" || len(p.Products) != 4 || p.Products[1].Price.Amount != "12000" {
		t.Fatalf("products = %+v", p)
	}
}

func TestHostOverrides(t *testing.T) {
	b, _ := newBrowser(t)
	if res := b.get("http://fakeshop:8080/api/products?host=us-shop.test"); !strings.Contains(res.body, `"U-100"`) {
		t.Errorf("?host=: %d %s", res.status, res.body)
	}
	res := b.do("GET", "http://fakeshop:8080/admin/rates", nil, "", http.Header{"X-Forwarded-Host": {"rates.test"}})
	if !strings.Contains(res.body, `"USD/JPY":"150"`) {
		t.Errorf("X-Forwarded-Host: %d %s", res.status, res.body)
	}
	if res := b.get("http://nowhere.test/"); res.status != http.StatusNotFound {
		t.Errorf("unknown host: %d", res.status)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
