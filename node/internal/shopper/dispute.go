package shopper

import (
	"context"
	"fmt"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// onDisputeOpen records the copy of a dispute the user opened with the escrow.
func (e *Engine) onDisputeOpen(ctx context.Context, msg *messenger.Message) {
	var d proto.DisputeOpen
	if msg.Decode(&d) != nil {
		return
	}
	_, _ = e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.User {
			return errSkip
		}
		o.Dispute = &Dispute{OpenedBy: msg.From, Claim: d.Claim, Text: d.Text, At: time.Now().Unix()}
		o.Events["dispute"] = msg.Inner
		if !terminal(o.State) {
			o.set(StateDisputed, d.Claim+": "+d.Text)
		}
		return nil
	})
	e.log.Info("dispute opened by the user", "order", msg.OrderID, "claim", d.Claim)
}

// onEvidenceRequest answers the escrow of the order with everything we have (§4.7). Sending many messages and
// attachments takes a while, so it runs in the background.
func (e *Engine) onEvidenceRequest(ctx context.Context, msg *messenger.Message) {
	o, ok, err := e.Order(msg.OrderID)
	if err != nil || !ok || msg.From != o.Request.Escrow {
		return
	}
	e.background(ctx, "evidence", o.ID, func(ctx context.Context) { e.sendEvidence(ctx, o) })
}

func (e *Engine) sendEvidence(ctx context.Context, o *Order) {
	parts := splitEvidence(e.Evidence(o))
	for _, body := range parts {
		if _, err := e.send(ctx, o, o.Request.Escrow, proto.TypeDisputeEvidence, body); err != nil {
			e.fail(o.ID, "evidence not sent", err)
			return
		}
	}
	// the full screenshots and receipts, too large for one message (§4.9)
	chunks, err := proto.Chunks(e.PurchaseEvidence(o.ID))
	if err != nil {
		e.fail(o.ID, "attachments", err)
	}
	for _, c := range chunks {
		if _, err := e.send(ctx, o, o.Request.Escrow, proto.TypeAttachment, c); err != nil {
			e.fail(o.ID, "attachment not sent", err)
			return
		}
	}
	_, _ = e.update(o.ID, func(o *Order) error {
		o.note(fmt.Sprintf("evidence sent to the escrow: %d message(s), %d attachment chunk(s)", len(parts), len(chunks)))
		return nil
	})
}

// splitEvidence spreads the signed messages over several dispute.evidence bodies so that each stays
// under the message size limit. The first body carries everything else.
func splitEvidence(ev proto.DisputeEvidence) []proto.DisputeEvidence {
	const budget = proto.MaxInnerBytes / 2 // leaves room for the inner's own JSON escaping
	first := ev
	first.Messages = nil
	parts := []proto.DisputeEvidence{first}
	size := 0
	for _, m := range ev.Messages {
		n := len(m.String())
		cur := &parts[len(parts)-1]
		if size+n > budget && len(cur.Messages) > 0 {
			parts = append(parts, proto.DisputeEvidence{})
			cur, size = &parts[len(parts)-1], 0
		}
		cur.Messages = append(cur.Messages, m)
		size += n
	}
	return parts
}

// Evidence collects the signed messages of the order (among them the user's order.escrow_key), the tracking,
// the purchase evidence and the delivery key for the escrow. The escrow decrypts the ciphertext of the signed
// request itself.
func (e *Engine) Evidence(o *Order) proto.DisputeEvidence {
	var msgs []*nostr.Event
	seen := map[string]bool{}
	for _, list := range [][]*nostr.Event{e.Messenger.Inbox(o.ID), e.Messenger.Outbox(o.ID)} {
		for _, ev := range list {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				msgs = append(msgs, ev)
			}
		}
	}
	return proto.DisputeEvidence{
		Messages:             msgs,
		Tracking:             o.Tracking,
		DeliveryKeyForEscrow: o.EscrowKey,
		PurchaseEvidence:     proto.InlineOnly(e.PurchaseEvidence(o.ID)),
	}
}
