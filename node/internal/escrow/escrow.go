// Package escrow is the escrow side (spec §3.2, §4.7, §4.8): it records escrow.notice, opens a case on
// dispute.open, asks the other party for evidence, decrypts the delivery address with key_for_escrow, refuses
// cases whose upfront fee was not paid, and rules with a signed payout when the operator of the node asks.
package escrow

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
	CaseFeePending   = "fee_pending" // the upfront fee could not be checked yet; checked again periodically
	CaseOpen         = "open"
	CaseNoObligation = "no_obligation" // upfront fee not paid (or paid for another order): we do not rule
	CaseRuled        = "ruled"
	CaseClosed       = "closed"
)

const (
	bucketCases       = "cases"
	bucketNotices     = "notices"
	bucketFeeUses     = "fee_uses"          // upfront fee → order id: one fee pays for one order (§4.6)
	bucketAttachments = "attachments"       // <order>/<sha256> → attachmentMeta
	bucketChunks      = "attachment_chunks" // <order>/<sha256>/<index> → base64 chunk
	bucketAttachData  = "attachment_data"   // <order>/<sha256> → base64 of the whole item
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
	// Attachments are the full evidence items (screenshots, receipts) by sha256, sent in chunks (§4.9). They
	// are kept in their own buckets and only filled in when a case is read.
	Attachments map[string]*Attachment `json:"attachments,omitempty"`

	Ruling *proto.Ruling `json:"ruling,omitempty"`
	// ClaimedPayout is the transaction a party says carried out the ruling; the case closes once the chain
	// shows the escrow spent.
	ClaimedPayout string   `json:"claimed_payout,omitempty"`
	PayoutTx      string   `json:"payout_tx,omitempty"`
	History       []string `json:"history"`
}

// Attachment is one evidence item received in chunks; DataB64 is set once all chunks arrived and the hash
// matched.
type Attachment struct {
	From    string `json:"from"`
	MIME    string `json:"mime"`
	Total   int    `json:"total"`
	Have    int    `json:"have"`
	Size    int    `json:"size,omitempty"`
	DataB64 string `json:"data_b64,omitempty"`
}

// Bounds of what a party can make us store. Attachments may arrive before the evidence that names them (relays
// do not keep order), so they are kept by hash and matched when displayed.
const (
	maxAttachmentsPerParty = 4
	maxChunks              = 64 // 64 × 12 KiB = 768 KiB per item
	maxEvidencePerParty    = 32
)

func (c *Case) note(format string, args ...any) {
	c.Updated = time.Now().Unix()
	line := fmt.Sprintf(format, args...)
	// a check failing the same way on every retry does not grow the history
	if n := len(c.History); n > 0 && strings.HasSuffix(c.History[n-1], " "+line) {
		return
	}
	c.History = append(c.History, time.Now().UTC().Format(time.RFC3339)+" "+line)
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
	log    *slog.Logger
	mu     sync.Mutex // guards changes of case documents
	ruleMu sync.Mutex // one ruling at a time (a signed payout cannot be taken back)
	busy   sync.Map   // background checks in progress, by order id
	jobs   sync.WaitGroup
	base   atomic.Pointer[context.Context] // the context of Start, for background checks
}

// Wait waits for the background checks that are running.
func (e *Engine) Wait() { e.jobs.Wait() }

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

// Case loads one case with its attachments.
func (e *Engine) Case(id string) (*Case, bool, error) {
	c, ok, err := e.caseDoc(id)
	if ok && err == nil {
		c.Attachments = e.attachments(id, true)
	}
	return c, ok, err
}

func (e *Engine) caseDoc(id string) (*Case, bool, error) {
	var c Case
	ok, err := e.DB.Get(bucketCases, id, &c)
	return &c, ok, err
}

// Start runs the background checks until ctx ends: fees that could not be checked, and escrows that may have
// been spent (a ruling carried out, or a timelock branch taken).
func (e *Engine) Start(ctx context.Context) {
	e.base.Store(&ctx)
	go func() {
		t := time.NewTicker(WatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			e.watch(ctx)
		}
	}()
}

// WatchInterval is the period of the background checks.
var WatchInterval = 5 * time.Second

func (e *Engine) watch(ctx context.Context) {
	cases, err := e.Cases()
	if err != nil {
		return
	}
	for _, c := range cases {
		switch c.State {
		case CaseFeePending:
			e.checkFee(ctx, c.OrderID)
		case CaseOpen, CaseRuled:
			e.checkSpent(ctx, c.OrderID)
		}
	}
}

// background runs fn unless a check of the same order is running. fn gets the engine's context: the context of
// a message handler ends with the handler.
func (e *Engine) background(ctx context.Context, id string, fn func(ctx context.Context)) {
	if _, running := e.busy.LoadOrStore(id, struct{}{}); running {
		return
	}
	if base := e.base.Load(); base != nil {
		ctx = *base
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	e.jobs.Add(1)
	go func() {
		defer e.jobs.Done()
		defer e.busy.Delete(id)
		fn(ctx)
	}()
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
	// a later notice may complete or correct the funding, but never replace the request
	err = store.Modify(e.DB, bucketNotices, o.ID, func(old *notice, exists bool) error {
		if exists && len(old.Events) > 0 && old.Events[0].ID != o.RequestEvent.ID {
			return fmt.Errorf("a notice with another request (%s) is already recorded", old.Events[0].ID)
		}
		*old = notice{From: msg.From, Received: time.Now().Unix(), Events: o.Events()}
		return nil
	})
	if err != nil {
		e.log.Warn("escrow.notice not recorded", "order", o.ID, "err", err)
		return
	}
	e.log.Info("escrow.notice recorded", "order", o.ID, "asset", o.Quote.Asset, "lock", o.Quote.LockAmount)
}

// agreement assembles the signed order (§4.7): the messages of the escrow.notice when there is one (differing
// evidence messages are ignored and reported), else the evidence messages.
func (e *Engine) agreement(orderID string, evidence []*nostr.Event) (*contract.Order, []string, error) {
	var n notice
	_, _ = e.DB.Get(bucketNotices, orderID, &n)
	evs, ignored, err := contract.Select(orderID, n.Events, evidence)
	if err != nil {
		return nil, nil, err
	}
	o, err := contract.FromEvents(evs)
	return o, ignored, err
}

func (e *Engine) onDisputeOpen(ctx context.Context, msg *messenger.Message) {
	var d proto.DisputeOpen
	if err := msg.Decode(&d); err != nil {
		return
	}
	e.mu.Lock()
	if c, ok, _ := e.caseDoc(msg.OrderID); ok {
		e.mu.Unlock()
		e.log.Info("dispute already open", "order", msg.OrderID, "state", c.State)
		if c.State == CaseFeePending && (msg.From == c.User || msg.From == c.Shopper) {
			e.background(ctx, c.OrderID, func(ctx context.Context) { e.checkFee(ctx, c.OrderID) })
		}
		return
	}
	defer e.mu.Unlock()
	o, ignored, err := e.agreement(msg.OrderID, d.Evidence.Messages)
	if err != nil {
		e.log.Warn("dispute without a verifiable order", "order", msg.OrderID, "err", err)
		return
	}
	if o.Request.Escrow != e.Keys.NostrPubHex() || (msg.From != o.User && msg.From != o.Shopper) {
		e.log.Warn("dispute from a stranger or for another escrow", "order", msg.OrderID, "from", msg.From)
		return
	}
	c := &Case{
		OrderID: o.ID, State: CaseFeePending, Opened: time.Now().Unix(), OpenedBy: msg.From, User: o.User, Shopper: o.Shopper,
		Asset: o.Quote.Asset, Claim: d.Claim, Text: d.Text, RequestedSplit: d.RequestedSplit, Agreement: o.Events(),
		Evidence: map[string][]proto.DisputeEvidence{msg.From: {d.Evidence}},
	}
	c.note("opened by %s: %s", msg.From, d.Claim)
	for _, ig := range ignored {
		c.note("evidence message %s differs from the escrow.notice, ignored", ig)
	}
	e.decryptAddress(c, o, d.Evidence)
	if err := e.DB.Put(bucketCases, c.ID(), c); err != nil {
		e.log.Error("store case", "err", err)
		return
	}
	e.log.Info("case opened", "order", c.OrderID, "by", msg.From)
	// the fee check asks the chain; it must not hold up the messages
	e.background(ctx, c.OrderID, func(ctx context.Context) { e.checkFee(ctx, c.OrderID) })
}

// checkFee decides whether we owe a ruling (§3.2): the upfront fee was paid, for this order only. A fee that
// could not be checked (network, chain not caught up) keeps the case pending, to be checked again.
func (e *Engine) checkFee(ctx context.Context, id string) {
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok || c.State != CaseFeePending {
		return
	}
	o, err := contract.FromEvents(c.Agreement)
	if err != nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	use, err := e.feePaid(fctx, o)
	cancel()
	if err == nil && use != "" {
		err = e.claimFee(use, id)
	}
	e.mu.Lock()
	var opened bool
	_ = store.Modify(e.DB, bucketCases, id, func(c *Case, exists bool) error {
		if !exists || c.State != CaseFeePending {
			return store.ErrStop
		}
		switch {
		case err == nil:
			c.FeePaid, c.State, opened = true, CaseOpen, true
			c.note("upfront fee paid")
		case contract.IsDefinite(err):
			c.State = CaseNoObligation
			c.note("upfront fee not paid (%v): no obligation to rule", err)
		default:
			c.note("upfront fee could not be checked yet: %v", err)
		}
		return nil
	})
	e.mu.Unlock()
	if !opened {
		return
	}
	other := o.Shopper
	if c.OpenedBy == o.Shopper {
		other = o.User
	}
	want := proto.EvidenceRequest{Want: []string{"messages", "tracking", "purchase_evidence", "delivery_key_for_escrow"}}
	if _, err := e.Messenger.Send(ctx, other, o.ID, proto.TypeDisputeEvidenceRequest, want, o.Request.Relays); err != nil {
		e.log.Warn("evidence request not sent", "order", o.ID, "err", err)
	}
}

// claimFee records that a fee pays for this order; a fee already counted for another order does not count.
func (e *Engine) claimFee(use, id string) error {
	return store.Modify(e.DB, bucketFeeUses, use, func(owner *string, exists bool) error {
		if exists && *owner != id {
			return contract.Mismatchf("%s already paid for order %s", use, *owner)
		}
		if exists {
			return store.ErrStop
		}
		*owner = id
		return nil
	})
}

// ID is the key of the case.
func (c *Case) ID() string { return c.OrderID }

// feePaid checks the upfront fee of §4.6 and returns the key of the fee for claimFee. Definite errors
// (contract.IsDefinite) mean not paid; others mean it could not be checked.
func (e *Engine) feePaid(ctx context.Context, o *contract.Order) (string, error) {
	if o.Funded == nil {
		return "", contract.Mismatchf("no order.funded")
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if o.Quote.EscrowBTCFeeAddress != e.Keys.WalletAddress() {
			return "", contract.Mismatchf("the fee was quoted to another address")
		}
		if e.BTC == nil {
			return "", errors.New("no bitcoin backend")
		}
		return contract.VerifyBTCFee(ctx, e.BTC, o.Quote, o.Funded)
	case proto.AssetUSDC:
		if e.EVM == nil || e.Deployments == nil {
			return "", errors.New("no EVM backend")
		}
		if !common.IsHexAddress(o.Quote.EscrowEVMAddress) || common.HexToAddress(o.Quote.EscrowEVMAddress) != e.Keys.EVMAddress() {
			return "", contract.Mismatchf("the fee was quoted to another address")
		}
		d, err := e.Deployments()
		if err != nil {
			return "", err
		}
		return contract.VerifyUSDCFee(ctx, e.EVM, d, o.Request, o.Quote, o.Funded, 1)
	}
	return "", contract.Mismatchf("unknown asset %q", o.Quote.Asset)
}

// decryptAddress opens the delivery address with key_for_escrow: from delivery_key_for_escrow of the evidence
// or from the user's signed order.escrow_key among its messages. The key must match key_for_escrow_sha256 of the
// signed request, and only the ciphertext of that request is decrypted (§4.4).
func (e *Engine) decryptAddress(c *Case, o *contract.Order, ev proto.DisputeEvidence) {
	if c.DeliveryAddress != nil || o == nil {
		return
	}
	candidates := []string{ev.DeliveryKeyForEscrow}
	for _, m := range ev.Messages {
		if m == nil || m.PubKey != c.User || giftwrap.OrderID(m) != c.OrderID || giftwrap.Type(m) != proto.TypeOrderEscrowKey || giftwrap.VerifyInner(m) != nil {
			continue
		}
		var k proto.EscrowKey
		if json.Unmarshal([]byte(m.Content), &k) == nil {
			candidates = append(candidates, k.KeyForEscrow)
		}
	}
	for _, key := range candidates {
		if key == "" {
			continue
		}
		if proto.EscrowKeyHash(key) != o.Request.Delivery.KeyForEscrowSHA256 {
			c.note("a delivery key that does not match key_for_escrow_sha256 was ignored")
			continue
		}
		k, err := delivery.UnwrapKey(e.Keys.NostrSecretHex(), c.User, key)
		if err != nil {
			c.note("delivery key does not open: %v", err)
			continue
		}
		addr, err := delivery.Open(k, o.Request.Delivery.Ciphertext, c.OrderID)
		if err != nil {
			c.note("delivery address does not decrypt: %v", err)
			continue
		}
		c.DeliveryAddress = &addr
		c.note("delivery address decrypted")
		return
	}
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
			if m == nil || giftwrap.VerifyInner(m) != nil {
				c.note("evidence of %s contains an invalid message, ignored", msg.From)
				return nil
			}
		}
		if c.Evidence == nil {
			c.Evidence = map[string][]proto.DisputeEvidence{}
		}
		if len(c.Evidence[msg.From]) >= maxEvidencePerParty {
			c.note("evidence of %s beyond %d messages, ignored", msg.From, maxEvidencePerParty)
			return nil
		}
		c.Evidence[msg.From] = append(c.Evidence[msg.From], ev)
		c.note("evidence from %s: %d messages, %d tracking entries", msg.From, len(ev.Messages), len(ev.Tracking))
		if o, err := contract.FromEvents(c.Agreement); err == nil {
			e.decryptAddress(c, o, ev)
		}
		return nil
	})
}

// attachmentMeta is the stored state of one attachment (the chunks and the data are stored apart).
type attachmentMeta struct {
	From  string `json:"from"`
	MIME  string `json:"mime"`
	Total int    `json:"total"`
	Have  int    `json:"have"`
	Size  int    `json:"size,omitempty"`
	Done  bool   `json:"done"`
}

func attachmentKey(order, sha string) string { return order + "/" + sha }

func (e *Engine) onAttachment(ctx context.Context, msg *messenger.Message) {
	var a proto.Attachment
	if err := msg.Decode(&a); err != nil || a.Total <= 0 || a.Total > maxChunks || a.Index < 0 || a.Index >= a.Total ||
		!isSHA256(a.SHA256) || len(a.MIME) > 100 || a.DataB64 == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok, err := e.caseDoc(msg.OrderID)
	if err != nil || !ok || (msg.From != c.User && msg.From != c.Shopper) {
		return
	}
	key := attachmentKey(c.OrderID, a.SHA256)
	var meta attachmentMeta
	if exists, _ := e.DB.Get(bucketAttachments, key, &meta); !exists {
		n := 0
		for _, at := range e.attachments(c.OrderID, false) {
			if at.From == msg.From {
				n++
			}
		}
		if n >= maxAttachmentsPerParty {
			return
		}
		meta = attachmentMeta{From: msg.From, MIME: a.MIME, Total: a.Total}
	}
	if meta.Done || meta.From != msg.From || meta.Total != a.Total {
		return
	}
	chunkKey := fmt.Sprintf("%s/%04d", key, a.Index)
	if e.DB.Has(bucketChunks, chunkKey) {
		return
	}
	if err := e.DB.Put(bucketChunks, chunkKey, a.DataB64); err != nil {
		return
	}
	meta.Have++
	if meta.Have == meta.Total {
		e.assemble(c.OrderID, a.SHA256, &meta)
		if !meta.Done {
			return
		}
	}
	_ = e.DB.Put(bucketAttachments, key, meta)
}

// assemble joins the chunks of a complete attachment and checks its hash; a bad one is dropped.
func (e *Engine) assemble(order, sha string, meta *attachmentMeta) {
	key := attachmentKey(order, sha)
	chunks := map[int]string{}
	for i := 0; i < meta.Total; i++ {
		var part string
		if ok, _ := e.DB.Get(bucketChunks, fmt.Sprintf("%s/%04d", key, i), &part); ok {
			chunks[i] = part
		}
	}
	data, ok, err := proto.Assemble(chunks, meta.Total, sha)
	for i := 0; i < meta.Total; i++ {
		_ = e.DB.Delete(bucketChunks, fmt.Sprintf("%s/%04d", key, i))
	}
	_ = store.Modify(e.DB, bucketCases, order, func(c *Case, _ bool) error {
		switch {
		case err != nil || !ok:
			c.note("attachment %s from %s rejected: %v", sha[:12], meta.From, err)
		default:
			c.note("attachment %s (%s, %d bytes) received", sha[:12], meta.MIME, len(data))
		}
		return nil
	})
	if err != nil || !ok {
		_ = e.DB.Delete(bucketAttachments, key)
		return
	}
	if e.DB.Put(bucketAttachData, key, base64.StdEncoding.EncodeToString(data)) == nil {
		meta.Done, meta.Size = true, len(data)
	}
}

// attachments lists the attachments of a case by sha256; withData adds the data of complete ones.
func (e *Engine) attachments(order string, withData bool) map[string]*Attachment {
	out := map[string]*Attachment{}
	prefix := order + "/"
	_ = e.DB.ForEach(bucketAttachments, func(key string, raw []byte) error {
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		var m attachmentMeta
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		out[strings.TrimPrefix(key, prefix)] = &Attachment{From: m.From, MIME: m.MIME, Total: m.Total, Have: m.Have, Size: m.Size}
		return nil
	})
	if withData {
		for sha, at := range out {
			var data string
			if ok, _ := e.DB.Get(bucketAttachData, attachmentKey(order, sha), &data); ok {
				at.DataB64 = data
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}

// onCountersigned notes the payout transaction a party names. The case closes only when the chain shows the
// escrow spent (checkSpent); a ruling must exist.
func (e *Engine) onCountersigned(ctx context.Context, msg *messenger.Message) {
	var ref proto.TxRef
	if msg.Decode(&ref) != nil || ref.TxID == "" || len(ref.TxID) > 66 {
		return
	}
	e.mu.Lock()
	var changed bool
	_ = store.Modify(e.DB, bucketCases, msg.OrderID, func(c *Case, exists bool) error {
		if !exists || (msg.From != c.User && msg.From != c.Shopper) || c.State != CaseRuled || c.Ruling == nil {
			return store.ErrStop
		}
		c.ClaimedPayout, changed = ref.TxID, true
		c.note("%s says the ruling was countersigned: %s", msg.From, ref.TxID)
		return nil
	})
	e.mu.Unlock()
	if changed {
		e.background(ctx, msg.OrderID, func(ctx context.Context) { e.checkSpent(ctx, msg.OrderID) })
	}
}

// checkSpent closes a case whose escrow the chain shows spent: by the ruling (the named transaction) or by any
// other spend (a timelock branch, a cooperative payout).
func (e *Engine) checkSpent(ctx context.Context, id string) {
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok || (c.State != CaseOpen && c.State != CaseRuled) {
		return
	}
	o, err := contract.FromEvents(c.Agreement)
	if err != nil || o.Funded == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var by string
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if e.BTC == nil || o.Funded.Vout == nil {
			return
		}
		sp, err := e.BTC.Outspend(cctx, o.Funded.TxID, *o.Funded.Vout)
		if err != nil || !sp.Spent {
			return
		}
		by = sp.TxID
	case proto.AssetUSDC:
		if e.EVM == nil || e.Deployments == nil {
			return
		}
		d, err := e.Deployments()
		if err != nil {
			return
		}
		safe := common.HexToAddress(o.Funded.Safe)
		bal, err := e.EVM.BalanceOf(cctx, d.USDC, safe)
		if err != nil || bal.Sign() != 0 {
			return
		}
		by = "(safe emptied)"
		if c.ClaimedPayout != "" {
			if r, err := e.EVM.Receipt(cctx, common.HexToHash(c.ClaimedPayout)); err == nil && r.Status == types.ReceiptStatusSuccessful {
				for _, t := range evm.TransfersOf(r, d.USDC) {
					if t.From == safe {
						by = c.ClaimedPayout
					}
				}
			}
		}
	default:
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = store.Modify(e.DB, bucketCases, id, func(c *Case, exists bool) error {
		if !exists || (c.State != CaseOpen && c.State != CaseRuled) {
			return store.ErrStop
		}
		c.PayoutTx, c.State = by, CaseClosed
		if c.Ruling != nil && by == c.ClaimedPayout {
			c.note("ruling carried out: %s", by)
		} else {
			c.note("escrow spent by %s", by)
		}
		return nil
	})
}

// ErrNoObligation is returned when asked to rule a case whose upfront fee was not paid.
var ErrNoObligation = errors.New("upfront fee not paid: this escrow does not rule the case")

// ErrAlreadyRuled is returned for a second ruling: a signed payout cannot be taken back, so two rulings could
// both be carried out.
var ErrAlreadyRuled = errors.New("the case is already ruled")

// RuleRequest is the body of POST /cases/{id}/rule (amounts in base units: sats or USDC units).
type RuleRequest struct {
	User    string `json:"user"`
	Shopper string `json:"shopper"`
	Reason  string `json:"reason"`
}

// Rule builds and signs the payout of a ruling and sends dispute.ruling to both parties. The escrow fee is
// dispute_fee_bps of the escrow balance (after the reserve); user + shopper must equal the rest. A case is
// ruled once.
func (e *Engine) Rule(ctx context.Context, id string, r RuleRequest) (*proto.Ruling, error) {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok {
		return nil, fmt.Errorf("no case %s", id)
	}
	if c.State == CaseFeePending {
		e.checkFee(ctx, id)
		if c, _, err = e.caseDoc(id); err != nil {
			return nil, err
		}
	}
	switch c.State {
	case CaseNoObligation:
		return nil, ErrNoObligation
	case CaseFeePending:
		return nil, errors.New("the upfront fee could not be checked yet; try again")
	case CaseRuled:
		return nil, ErrAlreadyRuled
	case CaseClosed:
		if c.Ruling != nil {
			return nil, ErrAlreadyRuled
		}
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
	// record the ruling before it leaves: after a failure it must not be signed a second time
	e.mu.Lock()
	err = store.Modify(e.DB, bucketCases, id, func(c *Case, _ bool) error {
		if c.State != CaseOpen {
			return fmt.Errorf("case is %s", c.State)
		}
		c.Ruling, c.State = ruling, CaseRuled
		c.note("ruled: user %s, shopper %s, fee %s", ruling.Split.User, ruling.Split.Shopper, ruling.Split.EscrowFee)
		return nil
	})
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, to := range []string{o.User, o.Shopper} {
		if _, err := e.Messenger.Send(ctx, to, o.ID, proto.TypeDisputeRuling, ruling, o.Request.Relays); err != nil {
			return nil, fmt.Errorf("send ruling: %w", err)
		}
	}
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
	// the output must be the P2WSH of the request and quote, whatever the funded message claims
	addr, err := esc.Address()
	if err != nil {
		return nil, err
	}
	if o.Quote.EscrowAddress != addr {
		return nil, fmt.Errorf("quoted escrow address %s is not the script address %s", o.Quote.EscrowAddress, addr)
	}
	out, err := contract.EscrowOutput(tx, *o.Funded.Vout, addr)
	if err != nil {
		return nil, err
	}
	if spent, err := e.BTC.Outspend(ctx, o.Funded.TxID, *o.Funded.Vout); err != nil {
		return nil, fmt.Errorf("outspend: %w", err)
	} else if spent.Spent {
		return nil, fmt.Errorf("escrow already spent by %s", spent.TxID)
	}
	prev := btc.Outpoint{TxID: o.Funded.TxID, Vout: *o.Funded.Vout, Amount: out.Value}
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
	if !common.IsHexAddress(o.Funded.Safe) {
		return nil, errors.New("funded without a safe address")
	}
	// the Safe must be the predicted one with the owners, threshold and module of the order
	os, err := contract.SafeOf(d, o.Request, o.Quote)
	if err != nil {
		return nil, err
	}
	safe := common.HexToAddress(o.Funded.Safe)
	if err := contract.CheckSafe(ctx, e.EVM, d, os, o.ID, o.Quote, safe); err != nil {
		return nil, err
	}
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
