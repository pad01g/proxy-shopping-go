// Package orders keeps the shops' orders in memory.
package orders

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/catalog"
	"github.com/pad01g/proxy-shopping-go/fakeshop/internal/money"
)

// Order statuses (payment side; shipment is in Tracking).
const (
	StatusPendingPayment = "pending_payment"
	StatusPaid           = "paid"
)

// Payment methods recorded on an order.
const (
	PayCard = "card"
	PayCash = "cash"
)

type Address struct {
	Name       string `json:"name"`
	PostalCode string `json:"postal_code"`
	Address    string `json:"address"`
	Phone      string `json:"phone"`
}

func (a Address) Validate() error {
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.PostalCode) == "" ||
		strings.TrimSpace(a.Address) == "" || strings.TrimSpace(a.Phone) == "" {
		return errors.New("shipping address needs name, postal_code, address and phone")
	}
	return nil
}

// Line is a requested SKU and quantity.
type Line struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// Item is an ordered line with its price resolved from the catalog.
type Item struct {
	SKU       string
	Name      string
	Qty       int
	UnitPrice money.Money
}

func (it Item) Subtotal() money.Money { return it.UnitPrice.Mul(it.Qty) }

type Order struct {
	ID        string
	Shop      string
	Status    string
	Payment   string
	Items     []Item
	Subtotal  money.Money
	Shipping  money.Money
	Total     money.Money
	Address   Address
	CreatedAt time.Time
	PaidAt    *time.Time
	ChargeID  string // card payments
	ReceiptNo string // cash payments

	carrier string
	manual  *manualStep
}

var (
	ErrNotFound  = errors.New("order not found")
	ErrNotPaid   = errors.New("order is not paid")
	ErrBackwards = errors.New("tracking cannot move backwards")
)

// Store is safe for concurrent use. Returned orders are copies.
type Store struct {
	mu     sync.Mutex
	now    func() time.Time
	orders map[string]*Order
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{now: now, orders: map[string]*Order{}}
}

// PriceLines resolves lines against the shop catalog. Unknown SKUs and
// non-positive quantities are rejected.
func PriceLines(shop *catalog.Shop, lines []Line) ([]Item, money.Money, error) {
	if len(lines) == 0 {
		return nil, money.Money{}, errors.New("no items")
	}
	subtotal := money.New(0, shop.Currency)
	items := make([]Item, 0, len(lines))
	for _, l := range lines {
		p, ok := shop.Product(l.SKU)
		if !ok {
			return nil, money.Money{}, fmt.Errorf("unknown sku %q", l.SKU)
		}
		if l.Qty <= 0 || l.Qty > 99 {
			return nil, money.Money{}, fmt.Errorf("invalid quantity %d for %s", l.Qty, l.SKU)
		}
		it := Item{SKU: p.SKU, Name: p.Name, Qty: l.Qty, UnitPrice: p.Price}
		items = append(items, it)
		subtotal = subtotal.Add(it.Subtotal())
	}
	return items, subtotal, nil
}

// Create records a new order. Card orders start unpaid; cash orders are
// paid on the spot.
// ErrSoldOut is returned for SOLDOUT- SKUs: the order fails before any payment, which is how the lab
// exercises a purchase that the shopper cannot complete (and must refund).
var ErrSoldOut = errors.New("sold out")

func (s *Store) Create(shop *catalog.Shop, lines []Line, addr Address, payment string) (Order, error) {
	items, subtotal, err := PriceLines(shop, lines)
	if err != nil {
		return Order{}, err
	}
	if err := addr.Validate(); err != nil {
		return Order{}, err
	}
	for _, l := range lines {
		if strings.HasPrefix(l.SKU, "SOLDOUT-") {
			return Order{}, fmt.Errorf("%w: %s", ErrSoldOut, l.SKU)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	o := &Order{
		ID:        shop.OrderPrefix + "-" + randomHex(4),
		Shop:      shop.Host,
		Status:    StatusPendingPayment,
		Payment:   payment,
		Items:     items,
		Subtotal:  subtotal,
		Shipping:  shop.Shipping,
		Total:     subtotal.Add(shop.Shipping),
		Address:   addr,
		CreatedAt: now,
		carrier:   shop.Carrier,
	}
	if payment == PayCash {
		o.Status = StatusPaid
		o.PaidAt = &now
		o.ReceiptNo = "R" + now.Format("20060102") + "-" + randomHex(3)
	}
	s.orders[o.ID] = o
	return *o, nil
}

// MarkPaid records a successful card charge. It is idempotent for the same
// charge so a reloaded confirmation page does not fail.
func (s *Store) MarkPaid(id, chargeID string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	if o.Status == StatusPaid {
		if o.ChargeID != chargeID {
			return Order{}, errors.New("order already paid by another charge")
		}
		return *o, nil
	}
	now := s.now()
	o.Status = StatusPaid
	o.PaidAt = &now
	o.ChargeID = chargeID
	return *o, nil
}

// Get returns the order and its current tracking state.
func (s *Store) Get(id string) (Order, Tracking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, Tracking{}, ErrNotFound
	}
	return *o, o.trackingAt(s.now()), nil
}

// Advance forces the shipment to a later status (admin API).
func (s *Store) Advance(id, status string) (Tracking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Tracking{}, ErrNotFound
	}
	if o.Status != StatusPaid {
		return Tracking{}, ErrNotPaid
	}
	now := s.now()
	cur := o.trackingAt(now)
	if rank(status) < rank(cur.Status) || (rank(cur.Status) == 2 && status != cur.Status) {
		return Tracking{}, ErrBackwards
	}
	o.manual = &manualStep{status: status, at: now}
	return o.trackingAt(now), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToUpper(hex.EncodeToString(b))
}
