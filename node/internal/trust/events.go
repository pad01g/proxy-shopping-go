// Package trust keeps the signed delegations, lists and profiles of spec §2–§3 and computes the effective set
// of shopper × escrow combinations.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
)

// Event kinds.
const (
	KindDelegation     = 30500
	KindList           = 30501
	KindShopperProfile = 30502
	KindEscrowProfile  = 30503
	KindInboxRelays    = 10050
	KindStatus         = 5401
)

// IsTrustKind tells whether the kind belongs to the trust topic.
func IsTrustKind(k int) bool { return k == KindDelegation || k == KindList }

// IsProfileKind tells whether the kind belongs to the profiles topic.
func IsProfileKind(k int) bool {
	return k == KindShopperProfile || k == KindEscrowProfile || k == KindInboxRelays
}

// Tag returns the first value of a tag.
func Tag(ev *nostr.Event, name string) string {
	if t := ev.Tags.Find(name); len(t) >= 2 {
		return t[1]
	}
	return ""
}

// Version is the v tag (§2.1). Events without one (e.g. 10050 of other clients) use created_at.
func Version(ev *nostr.Event) int64 {
	if v := Tag(ev, "v"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		return -1
	}
	return int64(ev.CreatedAt)
}

// Newer tells whether a replaces b: higher version, or the same version and the smaller id.
func Newer(a, b *nostr.Event) bool {
	va, vb := Version(a), Version(b)
	if va != vb {
		return va > vb
	}
	return a.ID < b.ID
}

// Key identifies an addressable event: (kind, pubkey, d).
type Key struct {
	Kind   int
	PubKey string
	D      string
}

func (k Key) String() string { return fmt.Sprintf("%d:%s:%s", k.Kind, k.PubKey, k.D) }

// KeyOf returns the key of an event. 10050 is replaceable, its d is empty.
func KeyOf(ev *nostr.Event) Key {
	k := Key{Kind: ev.Kind, PubKey: ev.PubKey}
	if ev.Kind != KindInboxRelays {
		k.D = Tag(ev, "d")
	}
	return k
}

// Validate checks signature and structure of a trust or profile event.
func Validate(ev *nostr.Event) error {
	if !IsTrustKind(ev.Kind) && !IsProfileKind(ev.Kind) {
		return fmt.Errorf("kind %d is not a trust or profile event", ev.Kind)
	}
	if err := giftwrap.Verify(ev); err != nil {
		return err
	}
	if v := Tag(ev, "v"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
			return fmt.Errorf("bad version tag %q", v)
		}
	} else if ev.Kind == KindDelegation || ev.Kind == KindList {
		return errors.New("delegations and lists need a v tag")
	}
	switch ev.Kind {
	case KindDelegation:
		d := Tag(ev, "d")
		if !nostr.IsValidPublicKey(d) {
			return errors.New("delegation d must be the operator pubkey")
		}
		if p := Tag(ev, "p"); p != "" && p != d {
			return errors.New("delegation p and d differ")
		}
		if r := Tag(ev, "revoked"); r != "" && r != "true" && r != "false" {
			return fmt.Errorf("bad revoked tag %q", r)
		}
	case KindList:
		if Tag(ev, "d") == "" {
			return errors.New("list without d")
		}
		if _, err := ParseList(ev); err != nil {
			return err
		}
	case KindShopperProfile, KindEscrowProfile:
		if Tag(ev, "d") == "" {
			return errors.New("profile without d")
		}
		if !json.Valid([]byte(ev.Content)) {
			return errors.New("profile content is not JSON")
		}
	}
	return nil
}

// Network returns the network of an event: the network tag, else d for lists and profiles.
func Network(ev *nostr.Event) string {
	if n := Tag(ev, "network"); n != "" {
		return n
	}
	if ev.Kind == KindList || ev.Kind == KindShopperProfile || ev.Kind == KindEscrowProfile {
		return Tag(ev, "d")
	}
	return ""
}

// Revoked tells whether a delegation is revoked.
func Revoked(ev *nostr.Event) bool { return Tag(ev, "revoked") == "true" }

// Relay is a mailbox relay of a list.
type Relay struct {
	URL           string `json:"url"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

// Entry is one combination of a list.
type Entry struct {
	Region        string   `json:"region"`
	Shopper       string   `json:"shopper"`
	Escrow        string   `json:"escrow"`
	Shops         []string `json:"shops"`
	Payments      []string `json:"payments"`
	Tags          []string `json:"tags"`
	EscrowSLADays int      `json:"escrow_sla_days"`
}

// List is the content of kind 30501.
type List struct {
	Network  string          `json:"network"`
	Name     string          `json:"name"`
	Regions  []string        `json:"regions"`
	Relays   []Relay         `json:"relays"`
	Chain    json.RawMessage `json:"chain,omitempty"`
	Entries  []Entry         `json:"entries"`
	Donation json.RawMessage `json:"donation,omitempty"`
	ReportTo string          `json:"report_to,omitempty"`
}

// ParseList decodes the content of a list event.
func ParseList(ev *nostr.Event) (*List, error) {
	var l List
	if err := json.Unmarshal([]byte(ev.Content), &l); err != nil {
		return nil, fmt.Errorf("list content: %w", err)
	}
	for i, e := range l.Entries {
		if !nostr.IsValidPublicKey(e.Shopper) || !nostr.IsValidPublicKey(e.Escrow) || e.Region == "" {
			return nil, fmt.Errorf("list entry %d is incomplete", i)
		}
	}
	return &l, nil
}

// ShopperProfile is the content of kind 30502.
type ShopperProfile struct {
	Name         string          `json:"name"`
	Payments     []string        `json:"payments"`
	Currencies   []string        `json:"currencies"`
	CashRegions  []string        `json:"cash_regions"`
	Fee          json.RawMessage `json:"fee,omitempty"`
	MaxOrder     json.RawMessage `json:"max_order,omitempty"`
	DeliveryDays int64           `json:"delivery_days"`
	EVMAddress   string          `json:"evm_address,omitempty"`
	BTCAddress   string          `json:"btc_address,omitempty"`
	P2P          *P2PInfo        `json:"p2p,omitempty"`
}

// UpfrontFee is the escrow's fee at funding time (§3.2).
type UpfrontFee struct {
	BPS     int64  `json:"bps"`
	MinSats string `json:"min_sats"`
	MinUSDC string `json:"min_usdc"`
}

// EscrowProfile is the content of kind 30503.
type EscrowProfile struct {
	Name          string     `json:"name"`
	BTCXpub       string     `json:"btc_xpub"`
	BTCFeeAddress string     `json:"btc_fee_address"`
	EVMAddress    string     `json:"evm_address"`
	UpfrontFee    UpfrontFee `json:"upfront_fee"`
	DisputeFeeBPS int64      `json:"dispute_fee_bps"`
	P2P           *P2PInfo   `json:"p2p,omitempty"`
}

// P2PInfo is the libp2p contact of a node.
type P2PInfo struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

// InboxRelays returns the relay tags of a kind 10050 event.
func InboxRelays(ev *nostr.Event) []string {
	var out []string
	for t := range ev.Tags.FindAll("relay") {
		out = append(out, t[1])
	}
	return out
}

// Covers implements §2.5: X == R or X starts with R + "-".
func Covers(r, x string) bool {
	return x == r || strings.HasPrefix(x, r+"-")
}

// CoversAny tells whether any of the regions covers x.
func CoversAny(regions []string, x string) bool {
	for _, r := range regions {
		if Covers(r, x) {
			return true
		}
	}
	return false
}
