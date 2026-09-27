package orders

import (
	"testing"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
)

func TestPriceLines(t *testing.T) {
	shop := catalog.Shops()[0] // safe-shop.test
	items, subtotal, err := PriceLines(shop, []Line{{"A-100", 2}, {"A-200", 1}})
	if err != nil || len(items) != 2 || subtotal.Amount() != "18400" {
		t.Fatalf("got %v %v %v", items, subtotal, err)
	}
	for _, bad := range [][]Line{nil, {{"X", 1}}, {{"A-100", 0}}, {{"A-100", 100}}} {
		if _, _, err := PriceLines(shop, bad); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}

func TestUnpaidOrderDoesNotShip(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	s := NewStore(func() time.Time { return now })
	addr := Address{"a", "b", "c", "d"}
	o, err := s.Create(catalog.Shops()[0], []Line{{"A-100", 1}}, addr, PayCard)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, tr, _ := s.Get(o.ID); tr.Status != Processing {
		t.Fatalf("unpaid order shipped: %+v", tr)
	}
	if _, err := s.Advance(o.ID, Shipped); err != ErrNotPaid {
		t.Fatalf("advance unpaid: %v", err)
	}
}
