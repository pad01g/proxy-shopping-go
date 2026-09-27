package node

import (
	"encoding/json"
	"slices"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// hasOrderWith tells whether pk is a party of an order (or case) we keep: the messenger resends unacknowledged
// messages about the order only to such parties (§4.10; a rejected request is answered once), and does not rate
// limit what they send about it.
func (n *Node) hasOrderWith(pk, orderID string) bool {
	if orderID == "" {
		return false
	}
	switch {
	case n.shopper != nil:
		// a rejection is sent once (§4.10): WantsRetry is false for rejected and unknown orders
		if !n.shopper.WantsRetry(orderID) {
			return false
		}
		o, ok, err := n.shopper.Order(orderID)
		return err == nil && ok && (pk == o.User || pk == o.Request.Escrow)
	case n.escrow != nil:
		c, ok, err := n.escrow.Case(orderID)
		if err != nil || !ok {
			return false
		}
		if c.User == "" && c.Shopper == "" { // parties not known yet
			return true
		}
		return pk == c.User || pk == c.Shopper
	}
	return false
}

// accepts tells the messenger which messages of senders without an order with us a role takes (§4.10: the others
// are neither stored nor acked): a shopper takes order requests, an escrow the notices and disputes of orders
// naming it, an operator reports. Everything else needs an order or case.
func (n *Node) accepts(msg *messenger.Message) bool {
	me := n.keys.NostrPubHex()
	switch {
	case n.shopper != nil:
		return msg.Type == proto.TypeOrderRequest
	case n.escrow != nil:
		switch msg.Type {
		case proto.TypeEscrowNotice:
			var b proto.EscrowNotice
			return msg.Decode(&b) == nil && requestNames(b.Request, msg.OrderID, me)
		case proto.TypeDisputeOpen:
			var b proto.DisputeOpen
			if msg.Decode(&b) != nil {
				return false
			}
			return slices.ContainsFunc(b.Evidence.Messages, func(ev *nostr.Event) bool { return requestNames(ev, msg.OrderID, me) })
		}
	case n.operator != nil:
		return msg.Type == proto.TypeReport
	}
	return false
}

// requestNames tells whether ev is a signed order.request of the order that names escrow as its escrow.
func requestNames(ev *nostr.Event, orderID, escrow string) bool {
	if ev == nil || giftwrap.Type(ev) != proto.TypeOrderRequest || giftwrap.OrderID(ev) != orderID || giftwrap.VerifyInner(ev) != nil {
		return false
	}
	var r proto.OrderRequest
	return json.Unmarshal([]byte(ev.Content), &r) == nil && r.Escrow == escrow
}

// hints returns the relays a sender reads when it has no inbox relays (10050): the relays of its order request.
func (n *Node) hints(msg *messenger.Message) []string {
	if msg.Type == proto.TypeOrderRequest {
		var r proto.OrderRequest
		if msg.Decode(&r) == nil {
			return r.Relays
		}
	}
	switch {
	case n.shopper != nil:
		if o, ok, err := n.shopper.Order(msg.OrderID); err == nil && ok {
			return o.Relays
		}
	case n.escrow != nil:
		if c, ok, err := n.escrow.Case(msg.OrderID); err == nil && ok {
			for _, ev := range c.Agreement {
				var r proto.OrderRequest
				if ev != nil && giftwrap.Type(ev) == proto.TypeOrderRequest && json.Unmarshal([]byte(ev.Content), &r) == nil {
					return r.Relays
				}
			}
		}
	}
	return nil
}
