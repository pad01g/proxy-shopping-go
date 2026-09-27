package orders

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

// Tracking statuses, as reported by GET /api/orders/{id}.
const (
	Processing = "processing"
	Shipped    = "shipped"
	Delivered  = "delivered"
	Failed     = "failed"
)

// Auto-advance schedule from docs/lab.md: shipped 2 s after the order is
// paid, delivered (or failed, for FAIL- SKUs) 3 s after that.
const (
	ShipAfter    = 2 * time.Second
	DeliverAfter = 3 * time.Second
)

func rank(status string) int {
	switch status {
	case Processing:
		return 0
	case Shipped:
		return 1
	case Delivered, Failed:
		return 2
	}
	return -1
}

// ValidStatus reports whether s is a tracking status.
func ValidStatus(s string) bool { return rank(s) >= 0 }

// Tracking is the shipment state at a point in time.
type Tracking struct {
	Status     string `json:"status"`
	Carrier    string `json:"carrier,omitempty"`
	TrackingNo string `json:"tracking_no,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
}

// manualStep is a status forced by the admin API.
type manualStep struct {
	status string
	at     time.Time
}

// trackingAt derives the shipment state from the paid time, the admin
// overrides and the clock. The automatic schedule never moves a shipment
// backwards past a manual step, and terminal states stay terminal.
func (o *Order) trackingAt(now time.Time) Tracking {
	status, at := Processing, o.CreatedAt
	if o.PaidAt != nil {
		status, at = Processing, *o.PaidAt
		if shipAt := o.PaidAt.Add(ShipAfter); !now.Before(shipAt) {
			status, at = Shipped, shipAt
		}
		if doneAt := o.PaidAt.Add(ShipAfter + DeliverAfter); !now.Before(doneAt) {
			status, at = Delivered, doneAt
			if o.failsDelivery() {
				status = Failed
			}
		}
	}
	if m := o.manual; m != nil && rank(m.status) >= rank(status) {
		status, at = m.status, m.at
	}

	t := Tracking{Status: status, UpdatedAt: at.Unix()}
	if status != Processing {
		t.Carrier = o.carrier
		t.TrackingNo = trackingNumber(o.ID)
	}
	return t
}

func (o *Order) failsDelivery() bool {
	for _, it := range o.Items {
		if len(it.SKU) >= 5 && it.SKU[:5] == "FAIL-" {
			return true
		}
	}
	return false
}

// trackingNumber is a stable 12-digit number derived from the order id.
func trackingNumber(orderID string) string {
	h := sha256.Sum256([]byte(orderID))
	return fmt.Sprintf("%012d", binary.BigEndian.Uint64(h[:8])%1_000_000_000_000)
}
