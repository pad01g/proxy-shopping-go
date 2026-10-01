// Package proto defines the message types and bodies of spec §4.3–§4.8 and the shopper-bot API of §9.
package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
)

// Message types (the t tag of the inner event).
const (
	TypeAck                    = "ack"
	TypeOrderRequest           = "order.request"
	TypeOrderQuote             = "order.quote"
	TypeOrderAccept            = "order.accept"
	TypeOrderCancel            = "order.cancel"
	TypeOrderFunded            = "order.funded"
	TypeOrderEscrowKey         = "order.escrow_key"
	TypeEscrowNotice           = "escrow.notice"
	TypeOrderPurchased         = "order.purchased"
	TypeOrderShipping          = "order.shipping"
	TypeOrderRelease           = "order.release"
	TypeOrderRefund            = "order.refund"
	TypeOrderCompleted         = "order.completed"
	TypeDisputeOpen            = "dispute.open"
	TypeDisputeEvidenceRequest = "dispute.evidence_request"
	TypeDisputeEvidence        = "dispute.evidence"
	TypeDisputeRuling          = "dispute.ruling"
	TypeDisputeCountersigned   = "dispute.countersigned"
	TypeReport                 = "report"
	TypeChat                   = "chat"
)

// Payment methods / assets.
const (
	AssetBTC  = "btc-signet"
	AssetUSDC = "usdc-evm"
)

// Money is an amount with its currency; amounts are decimal strings.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type Ack struct {
	IDs []string `json:"ids"`
}

type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// Delivery is the sealed address of order.request. key_for_escrow itself travels separately in order.escrow_key
// (spec §4.4); the request only commits to it with its SHA-256.
type Delivery struct {
	Ciphertext         string `json:"ciphertext"`
	KeyForShopper      string `json:"key_for_shopper"`
	KeyForEscrowSHA256 string `json:"key_for_escrow_sha256"`
}

// EscrowKey is order.escrow_key (user → shopper, right after the request).
type EscrowKey struct {
	KeyForEscrow string `json:"key_for_escrow"`
}

// EscrowKeyHash is key_for_escrow_sha256: hex(SHA-256(key_for_escrow)).
func EscrowKeyHash(keyForEscrow string) string {
	h := sha256.Sum256([]byte(keyForEscrow))
	return hex.EncodeToString(h[:])
}

// OrderRequest is §4.4.
type OrderRequest struct {
	ShopURL        string   `json:"shop_url"`
	ShopRegion     string   `json:"shop_region"`
	Items          []Item   `json:"items"`
	Payment        string   `json:"payment"`
	Escrow         string   `json:"escrow"`
	Operator       string   `json:"operator"`
	Coordinator    string   `json:"coordinator"`
	Delivery       Delivery `json:"delivery"`
	KeyProof       string   `json:"key_proof"`
	UserBTCPubkey  string   `json:"user_btc_pubkey,omitempty"`
	UserBTCAddress string   `json:"user_btc_address,omitempty"`
	UserEVMAddress string   `json:"user_evm_address,omitempty"`
	Relays         []string `json:"relays,omitempty"`
	// ReplyP2P is the user's libp2p address (§4.4, §10), signed with the request.
	ReplyP2P *P2PContact `json:"reply_p2p,omitempty"`
}

// P2PContact is a libp2p address: the reply_p2p of an order.request (§4.4).
type P2PContact struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

// MaxReplyP2PAddrs bounds the addresses of a reply_p2p.
const MaxReplyP2PAddrs = 8

// Validate checks a reply_p2p: a libp2p peer id, and at most MaxReplyP2PAddrs multiaddrs (each at most 1024
// bytes) that end in that peer id when they end in one.
func (c *P2PContact) Validate() error {
	id, err := peer.Decode(c.PeerID)
	if err != nil {
		return fmt.Errorf("reply_p2p.peer_id: %w", err)
	}
	if len(c.Addrs) > MaxReplyP2PAddrs {
		return fmt.Errorf("reply_p2p has %d addrs, at most %d", len(c.Addrs), MaxReplyP2PAddrs)
	}
	for _, s := range c.Addrs {
		if len(s) > 1024 {
			return errors.New("reply_p2p address longer than 1024 bytes")
		}
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			return fmt.Errorf("reply_p2p address %q: %w", s, err)
		}
		if _, last := peer.SplitAddr(a); last != "" && last != id {
			return fmt.Errorf("reply_p2p address %q is not of peer %s", s, c.PeerID)
		}
	}
	return nil
}

type Price struct {
	Items      Money `json:"items"`
	Shipping   Money `json:"shipping"`
	ShopperFee Money `json:"shopper_fee"`
}

type FXSourceRate struct {
	Name string `json:"name"`
	Rate string `json:"rate"`
	At   int64  `json:"at"`
}

type FX struct {
	Pair    string         `json:"pair"`
	Rate    string         `json:"rate"`
	Sources []FXSourceRate `json:"sources"`
	At      int64          `json:"at"`
}

type Timelock struct {
	T1 int64 `json:"t1"`
	T2 int64 `json:"t2"`
}

// Reject reasons of order.quote.
const (
	RejectRisk        = "risk"
	RejectRegion      = "region"
	RejectPayment     = "payment"
	RejectLimit       = "limit"
	RejectUnavailable = "unavailable"
	RejectTrust       = "trust"
	RejectInvalid     = "invalid"
)

// OrderQuote is §4.5.
type OrderQuote struct {
	Accept              bool      `json:"accept"`
	RejectReason        string    `json:"reject_reason,omitempty"`
	Detail              string    `json:"detail,omitempty"`
	ExpiresAt           int64     `json:"expires_at,omitempty"`
	Price               *Price    `json:"price,omitempty"`
	FX                  *FX       `json:"fx,omitempty"`
	Asset               string    `json:"asset,omitempty"`
	LockAmount          string    `json:"lock_amount,omitempty"`
	EscrowUpfrontFee    string    `json:"escrow_upfront_fee,omitempty"`
	PayoutFeeReserve    string    `json:"payout_fee_reserve,omitempty"`
	Timelock            *Timelock `json:"timelock,omitempty"`
	ShopperBTCPubkey    string    `json:"shopper_btc_pubkey,omitempty"`
	ShopperBTCAddress   string    `json:"shopper_btc_address,omitempty"`
	EscrowBTCPubkey     string    `json:"escrow_btc_pubkey,omitempty"`
	EscrowBTCFeeAddress string    `json:"escrow_btc_fee_address,omitempty"`
	ShopperEVMAddress   string    `json:"shopper_evm_address,omitempty"`
	EscrowEVMAddress    string    `json:"escrow_evm_address,omitempty"`
	EscrowAddress       string    `json:"escrow_address,omitempty"`
}

type OrderAccept struct {
	QuoteID string `json:"quote_id"`
}

type OrderCancel struct {
	Reason string `json:"reason"`
}

// OrderFunded is §4.6 (BTC or USDC fields).
type OrderFunded struct {
	Asset    string  `json:"asset"`
	TxID     string  `json:"txid,omitempty"`
	Vout     *uint32 `json:"vout,omitempty"`
	Amount   string  `json:"amount"`
	FeeTxID  string  `json:"fee_txid,omitempty"`
	Safe     string  `json:"safe,omitempty"`
	DeployTx string  `json:"deploy_tx,omitempty"`
	FundTx   string  `json:"fund_tx,omitempty"`
	FeeTx    string  `json:"fee_tx,omitempty"`
}

type EscrowNotice struct {
	Request *nostr.Event `json:"request"`
	Quote   *nostr.Event `json:"quote"`
	Accept  *nostr.Event `json:"accept"`
	Funded  *nostr.Event `json:"funded"`
}

// Evidence is §9.
type Evidence struct {
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	MIME    string `json:"mime"`
	DataB64 string `json:"data_b64,omitempty"`
}

type OrderPurchased struct {
	ShopOrderID string     `json:"shop_order_id"`
	Total       Money      `json:"total"`
	Evidence    []Evidence `json:"evidence"`
}

// TrackingStatus is §9. updated_at is kept as sent by the bot.
type TrackingStatus struct {
	Status     string          `json:"status"`
	Carrier    string          `json:"carrier,omitempty"`
	TrackingNo string          `json:"tracking_no,omitempty"`
	UpdatedAt  json.RawMessage `json:"updated_at,omitempty"`
	Evidence   []Evidence      `json:"evidence"`
}

type OrderShipping struct {
	Status   string         `json:"status"`
	Tracking TrackingStatus `json:"tracking"`
}

// Release is order.release / order.refund: a partially signed payout.
type Release struct {
	Asset     string      `json:"asset"`
	PSBT      string      `json:"psbt,omitempty"`
	SafeTx    *evm.SafeTx `json:"safe_tx,omitempty"`
	Signature string      `json:"signature,omitempty"`
}

type TxRef struct {
	TxID string `json:"txid"`
}

type RequestedSplit struct {
	User    string `json:"user"`
	Shopper string `json:"shopper"`
}

// DisputeEvidence is the evidence of dispute.open and dispute.evidence. The escrow decrypts only the ciphertext
// of the signed request, so there is no ciphertext here.
type DisputeEvidence struct {
	Messages             []*nostr.Event   `json:"messages,omitempty"`
	Tracking             []TrackingStatus `json:"tracking,omitempty"`
	PurchaseEvidence     []Evidence       `json:"purchase_evidence,omitempty"`
	DeliveryKeyForEscrow string           `json:"delivery_key_for_escrow,omitempty"`
	Text                 string           `json:"text,omitempty"`
}

// Dispute claims.
const (
	ClaimNotDelivered = "not_delivered"
	ClaimWrongItem    = "wrong_item"
	ClaimNotReleased  = "not_released"
	ClaimOther        = "other"
)

type DisputeOpen struct {
	Claim          string          `json:"claim"`
	Text           string          `json:"text"`
	RequestedSplit *RequestedSplit `json:"requested_split,omitempty"`
	Evidence       DisputeEvidence `json:"evidence"`
}

type EvidenceRequest struct {
	Want []string `json:"want"`
}

type Split struct {
	User      string `json:"user"`
	Shopper   string `json:"shopper"`
	EscrowFee string `json:"escrow_fee"`
}

// Ruling is §4.8.
type Ruling struct {
	Split     Split       `json:"split"`
	Reason    string      `json:"reason"`
	Asset     string      `json:"asset"`
	PSBT      string      `json:"psbt,omitempty"`
	SafeTx    *evm.SafeTx `json:"safe_tx,omitempty"`
	Signature string      `json:"signature,omitempty"`
}

type Report struct {
	Subject  string         `json:"subject"`
	OrderID  string         `json:"order_id"`
	Text     string         `json:"text"`
	Evidence []*nostr.Event `json:"evidence,omitempty"`
}

type Chat struct {
	Text string `json:"text"`
}

// Shopper-bot API (§9).

type PurchaseRequest struct {
	RequestID  string `json:"request_id"`
	OrderID    string `json:"order_id"`
	ShopURL    string `json:"shop_url"`
	Items      []Item `json:"items"`
	Shipping   any    `json:"shipping"`
	PaymentRef string `json:"payment_ref"`
	MaxAmount  Money  `json:"max_amount"`
}

type PurchaseResult struct {
	RequestID   string     `json:"request_id"`
	Status      string     `json:"status"` // ok | failed | needs_human
	ShopOrderID string     `json:"shop_order_id,omitempty"`
	Total       *Money     `json:"total,omitempty"`
	Evidence    []Evidence `json:"evidence"`
	Error       string     `json:"error,omitempty"`
}

type TrackingQuery struct {
	ShopURL     string `json:"shop_url"`
	ShopOrderID string `json:"shop_order_id"`
}
