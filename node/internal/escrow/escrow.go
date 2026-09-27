// Package escrow is the escrow side (spec §3.2, §4.7, §4.8): it records escrow.notice, opens a case on
// dispute.open, asks the other party for evidence, decrypts the delivery address with key_for_escrow, refuses
// cases whose upfront fee was not paid, and rules with a signed payout when the operator of the node asks.
package escrow

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/amount"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// Case states.
const (
	CaseOpen         = "open"
	CaseNoObligation = "no_obligation" // upfront fee not paid: we do not rule
	CaseRuled        = "ruled"
	CaseClosed       = "closed"
)

const (
	bucketCases   = "cases"
	bucketNotices = "notices"
)

// Case is one dispute.
type Case struct {
	OrderID  string `json:"order_id"`
	State    string `json:"state"`
	Opened   int64  `json:"opened"`
	Updated  int64  `json:"updated"`
	OpenedBy string `json:"opened_by"`
	User     string `json:"user"`
	Shopper  string `json:"shopper"`
	Asset    string `json:"asset,omitempty"`
	Error    string `json:"error,omitempty"`

	Claim          string                `json:"claim"`
	Text           string                `json:"text"`
	RequestedSplit *proto.RequestedSplit `json:"requested_split,omitempty"`

	// Agreement are the signed request, quote, accept and funded messages.
	Agreement []*nostr.Event `json:"agreement,omitempty"`
	// Evidence per party (pubkey) in the order received.
	Evidence map[string][]proto.DisputeEvidence `json:"evidence"`
	// DeliveryAddress is decrypted with key_for_escrow.
	DeliveryAddress *delivery.Address `json:"delivery_address,omitempty"`
	FeePaid         bool              `json:"fee_paid"`
	// Attachments are the full evidence items (screenshots, receipts) by sha256, sent in chunks (§4.9).
	Attachments map[string]*Attachment `json:"attachments,omitempty"`

	Ruling   *proto.Ruling `json:"ruling,omitempty"`
	PayoutTx string        `json:"payout_tx,omitempty"`
	History  []string      `json:"history"`
}

// Attachment is one evidence item being received in chunks; Data is set once all chunks arrived and the
// hash matched.
type Attachment struct {
	MIME    string         `json:"mime"`
	Total   int            `json:"total"`
	Chunks  map[int]string `json:"chunks,omitempty"`
	DataB64 string         `json:"data_b64,omitempty"`
}

// maxAttachments bounds what a party can make us store. Attachments may arrive before the evidence that
// names them (relays do not keep order), so they are kept by hash and matched when displayed.
const maxAttachments = 16

func (c *Case) note(format string, args ...any) {
	c.Updated = time.Now().Unix()
	c.History = append(c.History, time.Now().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...))
}

// BTCChain is the Esplora subset the escrow uses.
type BTCChain interface {
	Tx(ctx context.Context, txid string) (*btc.Tx, error)
	Confirmations(ctx context.Context, txid string) (int64, error)
	Outspend(ctx context.Context, txid string, vout uint32) (*btc.Outspend, error)
}

// Deps are the collaborators of the engine.
type Deps struct {
	Keys        *keys.Set
	Messenger   *messenger.Messenger
	Trust       *trust.Store
	Network     string
	BTC         BTCChain
	EVM         *evm.Client
	Deployments func() (*evm.Deployments, error)
	DB          *store.DB
	Config      *config.Escrow
	Name        string
	Log         *slog.Logger
}

// Engine runs the escrow side.
type Engine struct {
	Deps
	log *slog.Logger
	mu  sync.Mutex
}

// New creates the engine and registers its handlers.
func New(d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	e := &Engine{Deps: d, log: d.Log.With("component", "escrow")}
	m := d.Messenger
	m.Handle(proto.TypeEscrowNotice, e.onNotice)
	m.Handle(proto.TypeDisputeOpen, e.onDisputeOpen)
	m.Handle(proto.TypeDisputeEvidence, e.onEvidence)
	m.Handle(proto.TypeAttachment, e.onAttachment)
	m.Handle(proto.TypeDisputeCountersigned, e.onCountersigned)
	m.HandleOther(func(_ context.Context, msg *messenger.Message) {
		e.log.Info("message", "type", msg.Type, "from", msg.From, "order", msg.OrderID)
	})
	return e
}

// Profile is the content of our kind 30503.
func (e *Engine) Profile(p2p *trust.P2PInfo) (trust.EscrowProfile, error) {
	xpub, err := e.Keys.EscrowXpub()
	if err != nil {
		return trust.EscrowProfile{}, err
	}
	f := e.Config.UpfrontFee
	return trust.EscrowProfile{
		Name: e.Name, BTCXpub: xpub, BTCFeeAddress: e.Keys.WalletAddress(), EVMAddress: e.Keys.EVMAddress().Hex(),
		UpfrontFee:    trust.UpfrontFee{BPS: f.BPS, MinSats: f.MinSats, MinUSDC: f.MinUSDC},
		DisputeFeeBPS: e.Config.DisputeFeeBPS, P2P: p2p,
	}, nil
}

// Cases lists the cases.
func (e *Engine) Cases() ([]Case, error) { return store.List[Case](e.DB, bucketCases) }

// Case loads one case.
func (e *Engine) Case(id string) (*Case, bool, error) {
	var c Case
	ok, err := e.DB.Get(bucketCases, id, &c)
	return &c, ok, err
}

type notice struct {
	From     string         `json:"from"`
	Received int64          `json:"received"`
	Events   []*nostr.Event `json:"events"`
}

func (e *Engine) onNotice(ctx context.Context, msg *messenger.Message) {
	var n proto.EscrowNotice
	if err := msg.Decode(&n); err != nil {
		return
	}
	evs := []*nostr.Event{n.Request, n.Quote, n.Accept, n.Funded}
	o, err := contract.FromEvents(evs)
	if err != nil || o.ID != msg.OrderID || o.User != msg.From || o.Request.Escrow != e.Keys.NostrPubHex() {
		e.log.Warn("invalid escrow.notice", "order", msg.OrderID, "err", err)
		return
	}
	if err := e.DB.Put(bucketNotices, o.ID, notice{From: msg.From, Received: time.Now().Unix(), Events: o.Events()}); err != nil {
		e.log.Error("store notice", "err", err)
		return
	}
	e.log.Info("escrow.notice recorded", "order", o.ID, "asset", o.Quote.Asset, "lock", o.Quote.LockAmount)
}

// agreement finds the signed order messages: from the notice, else among the evidence messages.
func (e *Engine) agreement(orderID string, extra []*nostr.Event) (*contract.Order, error) {
	var evs []*nostr.Event
	var n notice
	if ok, _ := e.DB.Get(bucketNotices, orderID, &n); ok {
		evs = append(evs, n.Events...)
	}
	for _, ev := range extra {
		if giftwrap.OrderID(ev) == orderID {
			switch giftwrap.Type(ev) {
			case proto.TypeOrderRequest, proto.TypeOrderQuote, proto.TypeOrderAccept, proto.TypeOrderFunded:
				evs = append(evs, ev)
			}
		}
	}
	// the later copy of each type wins; FromEvents checks that they belong together
	return contract.FromEvents(evs)
}

func (e *Engine) onDisputeOpen(ctx context.Context, msg *messenger.Message) {
	var d proto.DisputeOpen
	if err := msg.Decode(&d); err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok, _ := e.Case(msg.OrderID); ok {
		e.log.Info("dispute already open", "order", msg.OrderID)
		return
	}
	o, err := e.agreement(msg.OrderID, d.Evidence.Messages)
	if err != nil {
		e.log.Warn("dispute without a verifiable order", "order", msg.OrderID, "err", err)
		return
	}
	if o.Request.Escrow != e.Keys.NostrPubHex() || (msg.From != o.User && msg.From != o.Shopper) {
		e.log.Warn("dispute from a stranger or for another escrow", "order", msg.OrderID, "from", msg.From)
		return
	}
	c := &Case{
		OrderID: o.ID, State: CaseOpen, Opened: time.Now().Unix(), OpenedBy: msg.From, User: o.User, Shopper: o.Shopper,
		Asset: o.Quote.Asset, Claim: d.Claim, Text: d.Text, RequestedSplit: d.RequestedSplit, Agreement: o.Events(),
		Evidence: map[string][]proto.DisputeEvidence{msg.From: {d.Evidence}},
	}
	c.note("opened by %s: %s", msg.From, d.Claim)
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c.FeePaid, err = e.feePaid(fctx, o)
	cancel()
	if !c.FeePaid {
		c.State = CaseNoObligation
		c.note("upfront fee not paid (%v): no obligation to rule", err)
	}
	e.decryptAddress(c, d.Evidence)
	if err := e.DB.Put(bucketCases, c.ID(), c); err != nil {
		e.log.Error("store case", "err", err)
		return
	}
	e.log.Info("case opened", "order", c.OrderID, "state", c.State, "by", msg.From)
	if c.State == CaseNoObligation {
		return
	}
	other := o.Shopper
	if msg.From == o.Shopper {
		other = o.User
	}
	want := proto.EvidenceRequest{Want: []string{"messages", "tracking", "purchase_evidence", "delivery_key_for_escrow"}}
	if _, err := e.Messenger.Send(ctx, other, o.ID, proto.TypeDisputeEvidenceRequest, want, o.Request.Relays); err != nil {
		e.log.Warn("evidence request not sent", "order", o.ID, "err", err)
	}
}

// ID is the key of the case.
func (c *Case) ID() string { return c.OrderID }

func (e *Engine) feePaid(ctx context.Context, o *contract.Order) (bool, error) {
	if o.Funded == nil {
		return false, errors.New("no order.funded")
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if o.Quote.EscrowBTCFeeAddress != e.Keys.WalletAddress() {
			return false, errors.New("the fee was quoted to another address")
		}
		if e.BTC == nil {
			return false, errors.New("no bitcoin backend")
		}
		err := contract.VerifyBTCFee(ctx, e.BTC, o.Quote, o.Funded)
		return err == nil, err
	case proto.AssetUSDC:
		if e.EVM == nil || e.Deployments == nil {
			return false, errors.New("no EVM backend")
		}
		if !common.IsHexAddress(o.Quote.EscrowEVMAddress) || common.HexToAddress(o.Quote.EscrowEVMAddress) != e.Keys.EVMAddress() {
			return false, errors.New("the fee was quoted to another address")
		}
		d, err := e.Deployments()
		if err != nil {
			return false, err
		}
		err = contract.VerifyUSDCFee(ctx, e.EVM, d, o.Quote, o.Funded)
		return err == nil, err
	}
	return false, fmt.Errorf("unknown asset %q", o.Quote.Asset)
}

// decryptAddress opens the delivery address with key_for_escrow when the evidence carries it.
func (e *Engine) decryptAddress(c *Case, ev proto.DisputeEvidence) {
	if c.DeliveryAddress != nil || ev.DeliveryKeyForEscrow == "" {
		return
	}
	ct := ev.DeliveryCiphertext
	if ct == "" {
		if o, err := contract.FromEvents(c.Agreement); err == nil {
			ct = o.Request.Delivery.Ciphertext
		}
	}
	k, err := delivery.UnwrapKey(e.Keys.NostrSecretHex(), c.User, ev.DeliveryKeyForEscrow)
	if err != nil {
		c.note("delivery key does not open: %v", err)
		return
	}
	addr, err := delivery.Open(k, ct, c.OrderID)
	if err != nil {
		c.note("delivery address does not decrypt: %v", err)
		return
	}
	c.DeliveryAddress = &addr
	c.note("delivery address decrypted")
}

func (e *Engine) onEvidence(ctx context.Context, msg *messenger.Message) {
	var ev proto.DisputeEvidence
	if err := msg.Decode(&ev); err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = store.Modify(e.DB, bucketCases, msg.OrderID, func(c *Case, exists bool) error {
		if !exists || (msg.From != c.User && msg.From != c.Shopper) {
			return store.ErrStop
		}
		for _, m := range ev.Messages {
			if giftwrap.VerifyInner(m) != nil {
				c.note("evidence of %s contains an invalid message, ignored", msg.From)
				return nil
			}
		}
		if c.Evidence == nil {
			c.Evidence = map[string][]proto.DisputeEvidence{}
		}
		c.Evidence[msg.From] = append(c.Evidence[msg.From], ev)
		c.note("evidence from %s: %d messages, %d tracking entries", msg.From, len(ev.Messages), len(ev.Tracking))
		e.decryptAddress(c, ev)
		return nil
	})
}

func (e *Engine) onAttachment(ctx context.Context, msg *messenger.Message) {
	var a proto.Attachment
	if err := msg.Decode(&a); err != nil || a.Total <= 0 || a.Total > 256 || a.Index < 0 || a.Index >= a.Total {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = store.Modify(e.DB, bucketCases, msg.OrderID, func(c *Case, exists bool) error {
		if !exists || (msg.From != c.User && msg.From != c.Shopper) {
			return store.ErrStop
		}
		if c.Attachments == nil {
			c.Attachments = map[string]*Attachment{}
		}
		at := c.Attachments[a.SHA256]
		if at == nil {
			if len(c.Attachments) >= maxAttachments {
				return store.ErrStop
			}
			at = &Attachment{MIME: a.MIME, Total: a.Total, Chunks: map[int]string{}}
			c.Attachments[a.SHA256] = at
		}
		if at.DataB64 != "" || at.Total != a.Total {
			return store.ErrStop
		}
		at.Chunks[a.Index] = a.DataB64
		data, ok, err := proto.Assemble(at.Chunks, at.Total, a.SHA256)
		switch {
		case err != nil:
			c.note("attachment %s from %s rejected: %v", a.SHA256[:12], msg.From, err)
			delete(c.Attachments, a.SHA256)
		case ok:
			at.DataB64, at.Chunks = base64.StdEncoding.EncodeToString(data), nil
			c.note("attachment %s (%s, %d bytes) received", a.SHA256[:12], a.MIME, len(data))
		}
		return nil
	})
}

func (e *Engine) onCountersigned(ctx context.Context, msg *messenger.Message) {
	var ref proto.TxRef
	if msg.Decode(&ref) != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = store.Modify(e.DB, bucketCases, msg.OrderID, func(c *Case, exists bool) error {
		if !exists || (msg.From != c.User && msg.From != c.Shopper) {
			return store.ErrStop
		}
		c.PayoutTx, c.State = ref.TxID, CaseClosed
		c.note("ruling countersigned by %s: %s", msg.From, ref.TxID)
		return nil
	})
}

// ErrNoObligation is returned when asked to rule a case whose upfront fee was not paid.
var ErrNoObligation = errors.New("upfront fee not paid: this escrow does not rule the case")

// RuleRequest is the body of POST /cases/{id}/rule (amounts in base units: sats or USDC units).
type RuleRequest struct {
	User    string `json:"user"`
	Shopper string `json:"shopper"`
	Reason  string `json:"reason"`
}

// Rule builds and signs the payout of a ruling and sends dispute.ruling to both parties. The escrow fee is
// dispute_fee_bps of the escrow balance (after the reserve); user + shopper must equal the rest.
func (e *Engine) Rule(ctx context.Context, id string, r RuleRequest) (*proto.Ruling, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok, err := e.Case(id)
	if err != nil || !ok {
		return nil, fmt.Errorf("no case %s", id)
	}
	switch c.State {
	case CaseNoObligation:
		return nil, ErrNoObligation
	case CaseClosed:
		return nil, errors.New("case is closed")
	}
	o, err := contract.FromEvents(c.Agreement)
	if err != nil || o.Funded == nil {
		return nil, fmt.Errorf("case without funding: %v", err)
	}
	user, err := amount.ParseInt(r.User)
	if err != nil {
		return nil, fmt.Errorf("user: %w", err)
	}
	shopper, err := amount.ParseInt(r.Shopper)
	if err != nil {
		return nil, fmt.Errorf("shopper: %w", err)
	}
	var ruling *proto.Ruling
	switch o.Quote.Asset {
	case proto.AssetBTC:
		ruling, err = e.ruleBTC(ctx, o, user, shopper)
	case proto.AssetUSDC:
		ruling, err = e.ruleUSDC(ctx, o, user, shopper)
	default:
		err = fmt.Errorf("unknown asset %q", o.Quote.Asset)
	}
	if err != nil {
		return nil, err
	}
	ruling.Reason = r.Reason
	for _, to := range []string{o.User, o.Shopper} {
		if _, err := e.Messenger.Send(ctx, to, o.ID, proto.TypeDisputeRuling, ruling, o.Request.Relays); err != nil {
			return nil, fmt.Errorf("send ruling: %w", err)
		}
	}
	_ = store.Modify(e.DB, bucketCases, id, func(c *Case, _ bool) error {
		c.Ruling, c.State = ruling, CaseRuled
		c.note("ruled: user %s, shopper %s, fee %s", ruling.Split.User, ruling.Split.Shopper, ruling.Split.EscrowFee)
		return nil
	})
	e.log.Info("ruled", "order", id, "user", ruling.Split.User, "shopper", ruling.Split.Shopper, "fee", ruling.Split.EscrowFee)
	return ruling, nil
}

func (e *Engine) split(total, user, shopper *big.Int) (*big.Int, error) {
	fee := amount.BPS(total, e.Config.DisputeFeeBPS)
	rest := new(big.Int).Sub(total, fee)
	if got := new(big.Int).Add(user, shopper); got.Cmp(rest) != 0 {
		return nil, fmt.Errorf("user + shopper must be %s (balance %s minus dispute fee %s), got %s", rest, total, fee, got)
	}
	return fee, nil
}

func (e *Engine) ruleBTC(ctx context.Context, o *contract.Order, user, shopper *big.Int) (*proto.Ruling, error) {
	esc, err := contract.BTCEscrow(o.Request, o.Quote)
	if err != nil {
		return nil, err
	}
	if o.Funded.Vout == nil {
		return nil, errors.New("funded without vout")
	}
	if e.BTC == nil {
		return nil, errors.New("no bitcoin backend")
	}
	tx, err := e.BTC.Tx(ctx, o.Funded.TxID)
	if err != nil {
		return nil, fmt.Errorf("funding tx: %w", err)
	}
	if int(*o.Funded.Vout) >= len(tx.Vout) {
		return nil, errors.New("funding output missing")
	}
	if spent, err := e.BTC.Outspend(ctx, o.Funded.TxID, *o.Funded.Vout); err == nil && spent.Spent {
		return nil, fmt.Errorf("escrow already spent by %s", spent.TxID)
	}
	prev := btc.Outpoint{TxID: o.Funded.TxID, Vout: *o.Funded.Vout, Amount: tx.Vout[*o.Funded.Vout].Value}
	reserve, err := amount.ParseInt(o.Quote.PayoutFeeReserve)
	if err != nil {
		return nil, err
	}
	total := big.NewInt(prev.Amount - reserve.Int64())
	fee, err := e.split(total, user, shopper)
	if err != nil {
		return nil, err
	}
	p, err := esc.NewSpend(prev, []btc.Output{
		{Address: o.Request.UserBTCAddress, Amount: user.Int64()},
		{Address: o.Quote.ShopperBTCAddress, Amount: shopper.Int64()},
		{Address: e.Keys.WalletAddress(), Amount: fee.Int64()},
	}, btc.PathMultisig)
	if err != nil {
		return nil, err
	}
	key, err := e.Keys.EscrowOrderKey(o.ID)
	if err != nil {
		return nil, err
	}
	if !key.PubKey().IsEqual(esc.Escrow) {
		return nil, errors.New("the quoted escrow key is not ours")
	}
	if err := btc.Sign(p, key); err != nil {
		return nil, err
	}
	b64, err := btc.EncodePSBT(p)
	if err != nil {
		return nil, err
	}
	return &proto.Ruling{
		Split: proto.Split{User: user.String(), Shopper: shopper.String(), EscrowFee: fee.String()},
		Asset: proto.AssetBTC, PSBT: b64,
	}, nil
}

func (e *Engine) ruleUSDC(ctx context.Context, o *contract.Order, user, shopper *big.Int) (*proto.Ruling, error) {
	if e.EVM == nil || e.Deployments == nil {
		return nil, errors.New("no EVM backend")
	}
	d, err := e.Deployments()
	if err != nil {
		return nil, err
	}
	safe := common.HexToAddress(o.Funded.Safe)
	bal, err := e.EVM.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return nil, err
	}
	fee, err := e.split(bal, user, shopper)
	if err != nil {
		return nil, err
	}
	nonce, err := e.EVM.SafeNonce(ctx, safe)
	if err != nil {
		return nil, err
	}
	t, err := evm.SplitTx(d.Safe.MultiSendCallOnly, d.USDC, []evm.Transfer{
		{To: common.HexToAddress(o.Request.UserEVMAddress), Amount: user},
		{To: common.HexToAddress(o.Quote.ShopperEVMAddress), Amount: shopper},
		{To: e.Keys.EVMAddress(), Amount: fee},
	}, nonce)
	if err != nil {
		return nil, err
	}
	sig, err := evm.Sign(t.Hash(d.ChainID, safe), e.Keys.EVM)
	if err != nil {
		return nil, err
	}
	return &proto.Ruling{
		Split: proto.Split{User: user.String(), Shopper: shopper.String(), EscrowFee: fee.String()},
		Asset: proto.AssetUSDC, SafeTx: &t, Signature: "0x" + hex.EncodeToString(sig),
	}, nil
}
