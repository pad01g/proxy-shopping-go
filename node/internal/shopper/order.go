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
	StateRequested      = "requested"
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
	// ReplyP2P is the request's reply_p2p when it is valid (§4.4): where the user takes messages over libp2p.
	ReplyP2P *proto.P2PContact `json:"reply_p2p,omitempty"`

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

	// EscrowKey is key_for_escrow of the user's order.escrow_key, kept for a dispute (§4.4).
	EscrowKey string `json:"escrow_key,omitempty"`
	// FundingSince is when the order.funded being checked arrived.
	FundingSince int64 `json:"funding_since,omitempty"`
	// ChainExpiresAt is expires_at in chain time (USDC: the EVM clock may run ahead of ours).
	ChainExpiresAt int64 `json:"chain_expires_at,omitempty"`
	// Pending are the actions still to be carried out (quote, refund, release, ruling, claim); the tick loop
	// retries them until they are done or can never succeed.
	Pending map[string]*Action `json:"pending,omitempty"`
	// ClaimedPayout is a payout transaction the user told us about (dispute.countersigned). It is only believed
	// once the chain shows the escrow spent by it.
	ClaimedPayout string `json:"claimed_payout,omitempty"`
	// Donation is the donation of the list the order was quoted under (§2.3), fixed at quote time.
	Donation *Donation `json:"donation,omitempty"`
	// RulingDecided: the stored ruling was looked at with the dispute known (countersign pending, or declined).
	RulingDecided bool `json:"ruling_decided,omitempty"`
	// FundingExpired: the funding did not confirm in time; a funding that still confirms is given back.
	FundingExpired bool `json:"funding_expired,omitempty"`
	// NextFundingCheck paces the checks of a funding that did not confirm in time.
	NextFundingCheck int64 `json:"next_funding_check,omitempty"`

	PayoutTx string  `json:"payout_tx,omitempty"`
	PayoutBy string  `json:"payout_by,omitempty"` // release | ruling | timelock | other
	History  []Entry `json:"history"`
}

// Kinds of pending actions.
const (
	ActQuote   = "quote"
	ActRefund  = "refund"
	ActRelease = "release"
	ActRuling  = "ruling"
	ActClaim   = "claim"
)

// Action is a pending action of an order.
type Action struct {
	Event    *nostr.Event `json:"event,omitempty"` // the message that asked for it (order.release, dispute.ruling)
	Tx       string       `json:"tx,omitempty"`    // the transaction sent for it, while not known to be final
	Raw      string       `json:"raw,omitempty"`   // BTC: the signed transaction, to broadcast again if it drops out
	Stuck    string       `json:"stuck,omitempty"` // EVM: a sent transaction not mined in time, to be replaced
	Sent     int64        `json:"sent,omitempty"`
	Since    int64        `json:"since"`
	Attempts int          `json:"attempts,omitempty"`
	Next     int64        `json:"next,omitempty"` // not tried again before this time
	Error    string       `json:"error,omitempty"`
}

// Donation is the donation of a list (§2.3), with bps capped at 1%.
type Donation struct {
	BTCAddress string `json:"btc_address,omitempty"`
	EVMAddress string `json:"evm_address,omitempty"`
	BPS        int64  `json:"bps"`
}

func (o *Order) addPending(kind string, a *Action) {
	if o.Pending == nil {
		o.Pending = map[string]*Action{}
	}
	a.Since = time.Now().Unix()
	o.Pending[kind] = a
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

const (
	bucketOrders = "orders"
	// bucketUses maps the chain objects that fund an order (outpoint, Safe, fee tx) to the order (§4.6).
	bucketUses = "funding_uses"
	// bucketEvidence keeps the full purchase evidence (screenshots) out of the order document.
	bucketEvidence = "purchase_evidence"
	// bucketTrackingEvidence keeps the full evidence of the tracking updates (by order id).
	bucketTrackingEvidence = "tracking_evidence"
)

func (o *Order) set(state, detail string) {
	now := time.Now().Unix()
	o.State, o.Updated = state, now
	o.History = append(o.History, Entry{At: now, State: state, Detail: detail})
}

// note adds a history line. A line repeating the last one (a retry failing the same way every tick) only
// refreshes its time, so the history does not grow with every retry.
func (o *Order) note(detail string) {
	o.Updated = time.Now().Unix()
	if n := len(o.History); n > 0 && o.History[n-1].State == o.State && o.History[n-1].Detail == detail {
		o.History[n-1].At = o.Updated
		return
	}
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
