package shopper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// Limits of the escrow on attachments (lab.md: 4 per party, 64 chunks each); what is beyond them is not sent.
const (
	maxAttachments      = 4
	maxAttachmentChunks = 64
)

// onDisputeOpen records the copy of a dispute the user opened with the escrow, and looks at a ruling that came
// before it. A later copy does not replace the first, and never takes back what a ruling set going (§4.8).
func (e *Engine) onDisputeOpen(ctx context.Context, msg *messenger.Message) {
	var d proto.DisputeOpen
	if msg.Decode(&d) != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.User {
			return errSkip
		}
		if o.Dispute != nil && o.Dispute.OpenedBy != "" {
			return errSkip
		}
		o.Dispute = &Dispute{OpenedBy: msg.From, Claim: d.Claim, Text: d.Text, At: time.Now().Unix()}
		o.Events["dispute"] = msg.Inner
		if !terminal(o.State) && o.State != StateDisputed {
			o.set(StateDisputed, d.Claim+": "+d.Text)
		}
		e.decideRuling(o)
		return nil
	})
	if err == nil {
		e.log.Info("dispute opened by the user", "order", msg.OrderID, "claim", d.Claim)
		e.kick(ctx, msg.OrderID)
	}
}

// onEvidenceRequest answers the escrow of the order with everything we have (§4.7). The request of the order's
// escrow shows that a dispute is open, even if its copy never reached us; a stored ruling is looked at then.
// Sending many messages and attachments takes a while, so it runs in the background.
func (e *Engine) onEvidenceRequest(ctx context.Context, msg *messenger.Message) {
	o, err := e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.Request.Escrow {
			return errSkip
		}
		if o.Dispute == nil {
			o.Dispute = &Dispute{Claim: "(evidence request of the escrow)", At: time.Now().Unix()}
			if !terminal(o.State) && o.State != StateDisputed {
				o.set(StateDisputed, "the escrow asks for evidence")
			}
		}
		e.decideRuling(o)
		return nil
	})
	if err != nil {
		cur, ok, gerr := e.Order(msg.OrderID)
		if gerr != nil || !ok || msg.From != cur.Request.Escrow {
			return
		}
		o = cur
	}
	e.kick(ctx, o.ID)
	e.background(ctx, "evidence", o.ID, func(ctx context.Context) { e.sendEvidence(ctx, o) })
}

func (e *Engine) sendEvidence(ctx context.Context, o *Order) {
	ev, attach, skipped := e.evidenceParts(o)
	parts := splitEvidence(ev)
	for _, body := range parts {
		if _, err := e.send(ctx, o, o.Request.Escrow, proto.TypeDisputeEvidence, body); err != nil {
			e.fail(o.ID, "evidence not sent", err)
			return
		}
	}
	// the full screenshots and receipts, too large for one message (§4.9)
	chunks, err := chunkAll(attach)
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
		if skipped > 0 {
			o.note(fmt.Sprintf("%d evidence item(s) beyond the escrow's attachment limits not sent", skipped))
		}
		return nil
	})
}

// evidenceParts is the evidence to send and the items to send as attachments: every item whose data does not
// travel inline, within the escrow's limits (maxAttachments items of at most maxAttachmentChunks chunks; the
// purchase evidence first). When the first dispute.evidence (all but the messages) would still be too large,
// the largest inline data moves to the attachments too.
func (e *Engine) evidenceParts(o *Order) (proto.DisputeEvidence, []proto.Evidence, int) {
	ev := e.Evidence(o)
	full := append(e.PurchaseEvidence(o.ID), e.TrackingEvidence(o.ID)...)
	byHash := map[string]proto.Evidence{}
	for _, it := range full {
		if it.DataB64 != "" {
			byHash[it.SHA256] = it
		}
	}
	var attach []proto.Evidence
	var skipped int
	queued := map[string]bool{}
	add := func(it proto.Evidence) bool {
		if queued[it.SHA256] {
			return true
		}
		n := (base64.StdEncoding.DecodedLen(len(it.DataB64)) + proto.AttachmentChunk - 1) / proto.AttachmentChunk
		if len(attach) >= maxAttachments || n > maxAttachmentChunks {
			skipped++
			return false
		}
		queued[it.SHA256] = true
		attach = append(attach, it)
		return true
	}
	for _, it := range full {
		if it.DataB64 != "" && base64.StdEncoding.DecodedLen(len(it.DataB64)) > proto.InlineEvidenceMax {
			add(it)
		}
	}
	// the first part must fit one message: move inline data to attachments, largest first
	for firstSize(ev) > evidenceBudget {
		items := inlineItems(&ev)
		if len(items) == 0 {
			break
		}
		sort.Slice(items, func(i, j int) bool { return len(items[i].DataB64) > len(items[j].DataB64) })
		it := items[0]
		if f, ok := byHash[it.SHA256]; ok {
			add(f)
		} else {
			skipped++
		}
		it.DataB64 = ""
	}
	for firstSize(ev) > evidenceBudget && len(ev.Tracking) > 0 {
		ev.Tracking = ev.Tracking[1:] // the oldest updates go first; the escrow asks the bot's shop if it must
		skipped++
	}
	return ev, attach, skipped
}

// evidenceBudget is the size of one dispute.evidence body, leaving room for the inner's own JSON escaping.
const evidenceBudget = proto.MaxInnerBytes / 2

// firstSize is the size of the first part (the evidence without its messages).
func firstSize(ev proto.DisputeEvidence) int {
	ev.Messages = nil
	b, _ := json.Marshal(ev)
	return len(b)
}

// inlineItems points at the evidence items of ev that carry data.
func inlineItems(ev *proto.DisputeEvidence) []*proto.Evidence {
	var out []*proto.Evidence
	for i := range ev.PurchaseEvidence {
		if ev.PurchaseEvidence[i].DataB64 != "" {
			out = append(out, &ev.PurchaseEvidence[i])
		}
	}
	for i := range ev.Tracking {
		for j := range ev.Tracking[i].Evidence {
			if ev.Tracking[i].Evidence[j].DataB64 != "" {
				out = append(out, &ev.Tracking[i].Evidence[j])
			}
		}
	}
	return out
}

// chunkAll splits the data of every item into attachment messages (§4.9).
func chunkAll(items []proto.Evidence) ([]proto.Attachment, error) {
	var out []proto.Attachment
	for _, it := range items {
		data, err := base64.StdEncoding.DecodeString(it.DataB64)
		if err != nil {
			return out, fmt.Errorf("evidence %s: %w", it.SHA256, err)
		}
		total := (len(data) + proto.AttachmentChunk - 1) / proto.AttachmentChunk
		for i := 0; i < total; i++ {
			end := min((i+1)*proto.AttachmentChunk, len(data))
			out = append(out, proto.Attachment{SHA256: it.SHA256, MIME: it.MIME, Index: i, Total: total,
				DataB64: base64.StdEncoding.EncodeToString(data[i*proto.AttachmentChunk : end])})
		}
	}
	return out, nil
}

// splitEvidence spreads the signed messages over several dispute.evidence bodies so that each stays
// under the message size limit. The first body carries everything else.
func splitEvidence(ev proto.DisputeEvidence) []proto.DisputeEvidence {
	first := ev
	first.Messages = nil
	parts := []proto.DisputeEvidence{first}
	size := firstSize(ev)
	for _, m := range ev.Messages {
		n := len(m.String())
		cur := &parts[len(parts)-1]
		if size+n > evidenceBudget && (len(cur.Messages) > 0 || len(parts) == 1) {
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
// request itself. Data too large for a message is left out (only the hash stays; it goes as an attachment).
func (e *Engine) Evidence(o *Order) proto.DisputeEvidence {
	var msgs []*nostr.Event
	seen := map[string]bool{}
	for _, list := range [][]*nostr.Event{e.inbox(o.ID), e.Messenger.Outbox(o.ID)} {
		for _, ev := range list {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				msgs = append(msgs, ev)
			}
		}
	}
	tracking := make([]proto.TrackingStatus, len(o.Tracking))
	for i, t := range o.Tracking {
		t.Evidence = proto.InlineOnly(t.Evidence)
		tracking[i] = t
	}
	return proto.DisputeEvidence{
		Messages:             msgs,
		Tracking:             tracking,
		DeliveryKeyForEscrow: o.EscrowKey,
		PurchaseEvidence:     proto.InlineOnly(e.PurchaseEvidence(o.ID)),
	}
}
