// Package operator is the inbox of an operator node: it keeps the reports (§4.3 `report`) sent to it.
package operator

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
)

const bucketReports = "reports"

// Report is a stored report with its signed message.
type Report struct {
	ID       string       `json:"id"`
	From     string       `json:"from"`
	Received int64        `json:"received"`
	Report   proto.Report `json:"report"`
	// InvalidEvidence counts quoted messages whose signature did not verify.
	InvalidEvidence int          `json:"invalid_evidence"`
	Message         *nostr.Event `json:"message"`
}

// Inbox stores reports.
type Inbox struct {
	db  *store.DB
	log *slog.Logger
}

// New registers the report handler.
func New(m *messenger.Messenger, db *store.DB, log *slog.Logger) *Inbox {
	if log == nil {
		log = slog.Default()
	}
	in := &Inbox{db: db, log: log.With("component", "operator")}
	m.Handle(proto.TypeReport, in.onReport)
	return in
}

func (in *Inbox) onReport(_ context.Context, msg *messenger.Message) {
	var r proto.Report
	if err := msg.Decode(&r); err != nil {
		in.log.Warn("bad report", "from", msg.From, "err", err)
		return
	}
	rep := Report{ID: msg.Inner.ID, From: msg.From, Received: time.Now().Unix(), Report: r, Message: msg.Inner}
	for _, ev := range r.Evidence {
		if giftwrap.VerifyInner(ev) != nil {
			rep.InvalidEvidence++
		}
	}
	if err := in.db.Put(bucketReports, rep.ID, rep); err != nil {
		in.log.Error("store report", "err", err)
		return
	}
	in.log.Info("report received", "from", msg.From, "subject", r.Subject, "order", r.OrderID)
}

// Reports lists the reports, newest first.
func (in *Inbox) Reports() ([]Report, error) {
	out, err := store.List[Report](in.db, bucketReports)
	if out == nil {
		out = []Report{} // an empty inbox is [] in JSON, not null
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received > out[j].Received })
	return out, err
}
