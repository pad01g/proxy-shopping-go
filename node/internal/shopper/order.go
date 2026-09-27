package shopper

import (
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shop"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

// Order states of the shopper side (spec §11).
const (
	StateRejected       = "rejected"
	StateQuoted         = "quoted"
	StateAccepted       = "accepted"
	StateCancelled      = "cancelled"
	StateFunding        = "funding" // order.funded received, waiting for the chain
	StateFunded         = "funded"
	StatePurchasing     = "purchasing"
	StatePurchased      = "purchased"
	StatePurchaseFailed = "purchase_failed"
	StateNeedsHuman     = "needs_human"
	StateShipped        = "shipped"
	StateDelivered      = "delivered"
	StateShipFailed     = "shipping_failed"
	StateDisputed       = "disputed"
	StateCompleted      = "completed" // released by the user
	StateSettled        = "settled"   // paid out by a ruling
	StateClaimed        = "claimed"   // taken through the T1 branch
	StateClosed         = "closed"    // escrow spent by somebody else
)

// terminal states: the escrow is spent or never existed.
func terminal(s string) bool {
	switch s {
	case StateRejected, StateCancelled, StateCompleted, StateSettled, StateClaimed, StateClosed:
		return true
	}
	return false
}

// Order is the shopper's record of one order.
type Order struct {
	ID      string   `json:"id"`
	User    string   `json:"user"`
	State   string   `json:"state"`
	Created int64    `json:"created"`
	Updated int64    `json:"updated"`
	Error   string   `json:"error,omitempty"`
	Relays  []string `json:"relays,omitempty"` // where the user reads

	Request proto.OrderRequest      `json:"request"`
	Quote   *proto.OrderQuote       `json:"quote,omitempty"`
	Funded  *proto.OrderFunded      `json:"funded,omitempty"`
	Events  map[string]*nostr.Event `json:"events"` // request, quote, accept, funded, …

	Shop     *shop.Info    `json:"shop,omitempty"`
	Score    int           `json:"risk_score"`
	Outpoint *btc.Outpoint `json:"outpoint,omitempty"`
	Safe     string        `json:"safe,omitempty"`

	Purchase   *proto.PurchaseResult  `json:"purchase,omitempty"`
	Tracking   []proto.TrackingStatus `json:"tracking,omitempty"`
	ShipStatus string                 `json:"ship_status,omitempty"`

	Dispute *Dispute      `json:"dispute,omitempty"`
	Ruling  *proto.Ruling `json:"ruling,omitempty"`

	PayoutTx string  `json:"payout_tx,omitempty"`
	PayoutBy string  `json:"payout_by,omitempty"` // release | ruling | timelock | other
	History  []Entry `json:"history"`
}

// Dispute records a dispute.open we were told about.
type Dispute struct {
	OpenedBy string `json:"opened_by"`
	Claim    string `json:"claim"`
	Text     string `json:"text"`
	At       int64  `json:"at"`
}

// Entry is a line of the order history.
type Entry struct {
	At     int64  `json:"at"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

const bucketOrders = "orders"

func (o *Order) set(state, detail string) {
	now := time.Now().Unix()
	o.State, o.Updated = state, now
	o.History = append(o.History, Entry{At: now, State: state, Detail: detail})
}

func (o *Order) note(detail string) {
	o.Updated = time.Now().Unix()
	o.History = append(o.History, Entry{At: o.Updated, State: o.State, Detail: detail})
}

// Orders lists all orders.
func (e *Engine) Orders() ([]Order, error) { return store.List[Order](e.db, bucketOrders) }

// Order loads one order.
func (e *Engine) Order(id string) (*Order, bool, error) {
	var o Order
	ok, err := e.db.Get(bucketOrders, id, &o)
	return &o, ok, err
}

// update changes an order atomically; fn returning store.ErrStop leaves it unchanged.
func (e *Engine) update(id string, fn func(o *Order) error) (*Order, error) {
	var out Order
	err := store.Modify(e.db, bucketOrders, id, func(o *Order, exists bool) error {
		if !exists {
			return errUnknownOrder
		}
		if err := fn(o); err != nil {
			return err
		}
		out = *o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
