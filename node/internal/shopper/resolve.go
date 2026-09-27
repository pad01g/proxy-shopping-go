package shopper

import (
	"context"
	"errors"
	"fmt"

	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// ResolveRequest is the body of POST /orders/{id}/resolve: the operator of the node settles an order the bot
// could not finish (needs_human).
//
//	{"action": "purchased", "shop_order_id": "…", "total": {"amount", "currency"}}  bought by hand
//	{"action": "refund"}                                                           not bought: offer a refund
type ResolveRequest struct {
	Action      string       `json:"action"`
	ShopOrderID string       `json:"shop_order_id,omitempty"`
	Total       *proto.Money `json:"total,omitempty"`
}

// ErrNotResolvable is returned for orders that are not waiting for a human.
var ErrNotResolvable = errors.New("order is not waiting for a human")

// Resolve ends the needs_human state of an order.
func (e *Engine) Resolve(ctx context.Context, id string, r ResolveRequest) (*Order, error) {
	switch r.Action {
	case "purchased":
		if r.ShopOrderID == "" || len(r.ShopOrderID) > 128 {
			return nil, errors.New("shop_order_id is required")
		}
		// order.purchased names what was paid (§4.3)
		if err := validTotal(r.Total); err != nil {
			return nil, err
		}
		o, err := e.update(id, func(o *Order) error {
			if o.State != StateNeedsHuman {
				return ErrNotResolvable
			}
			res := proto.PurchaseResult{RequestID: o.ID, Status: "ok", ShopOrderID: r.ShopOrderID, Total: r.Total, Evidence: []proto.Evidence{}}
			if o.Purchase != nil {
				res.Evidence = o.Purchase.Evidence
			}
			o.Purchase = &res
			o.set(StatePurchased, r.ShopOrderID+" (resolved by hand)")
			return nil
		})
		if err != nil {
			return nil, err
		}
		body := proto.OrderPurchased{ShopOrderID: r.ShopOrderID, Total: *r.Total, Evidence: proto.InlineOnly(e.PurchaseEvidence(id))}
		ev, err := e.send(ctx, o, o.User, proto.TypeOrderPurchased, body)
		if err != nil {
			e.fail(id, "purchased not sent", err)
		} else {
			o, _ = e.update(id, func(o *Order) error { o.Events["purchased"] = ev; return nil })
		}
		return o, nil
	case "refund":
		o, ok, err := e.Order(id)
		if err != nil || !ok {
			return nil, errUnknownOrder
		}
		if o.State != StateNeedsHuman {
			return nil, ErrNotResolvable
		}
		e.abandon(ctx, id, "not bought (resolved by hand)")
		o, _, err = e.Order(id)
		return o, err
	}
	return nil, fmt.Errorf("action must be purchased or refund, not %q", r.Action)
}
