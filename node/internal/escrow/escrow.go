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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// RulingSent is the dispute.ruling inner sent to each party (pubkey → id); a party missing here gets it on
	// the next round of the background checks.
	RulingSent map[string]string `json:"ruling_sent,omitempty"`
	// ClaimedPayout is the transaction a party says carried out the ruling; the case closes once the chain
	// shows the escrow spent.
	ClaimedPayout string   `json:"claimed_payout,omitempty"`
	PayoutTx      string   `json:"payout_tx,omitempty"`
	History       []string `json:"history"`

	// Verified: the funding of Agreement was verified on chain against our own key for the order (§4.7). Until
	// then the case is a stub: Agreement is empty, and the agreements named for the order are Candidates.
	Verified   bool        `json:"verified"`
	Candidates []candidate `json:"candidates,omitempty"`
	// Claims are the dispute.open messages received; the case opens with the first one of a party of the
	// verified agreement.
	Claims []claim `json:"claims,omitempty"`
	// EvidenceIDs are the inner ids of the dispute.evidence messages taken (a resent message counts once).
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	// Checks and NextCheck pace the checks of a pending case (exponential backoff).
	Checks    int   `json:"checks,omitempty"`
	NextCheck int64 `json:"next_check,omitempty"`
}

// candidate is an agreement named for the order (by an escrow.notice or the evidence of a dispute.open) whose
// funding is not verified yet.
type candidate struct {
	RequestID string         `json:"request_id"`
	Events    []*nostr.Event `json:"events"`
	Failed    string         `json:"failed,omitempty"` // why it definitely does not verify
}

// claim is a dispute.open received.
type claim struct {
	From           string                `json:"from"`
	EventID        string                `json:"event_id"`
	RequestID      string                `json:"request_id,omitempty"` // of the agreement in its evidence, if any
	Claim          string                `json:"claim"`
	Text           string                `json:"text,omitempty"`
	RequestedSplit *proto.RequestedSplit `json:"requested_split,omitempty"`
	At             int64                 `json:"at"`
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
	maxCandidates          = 4 // agreements kept per order while none is verified
	maxClaims              = 8
	maxText                = 2000
)

// Pacing of the checks of pending cases: the first retry after CheckBase, doubling up to CheckMax; a case that
// could not be verified within PendingTTL owes nothing.
var (
	CheckBase  = 5 * time.Second
	CheckMax   = time.Hour
	PendingTTL = 7 * 24 * time.Hour
)

// watchParallel bounds the chain checks the background loop runs at once.
const watchParallel = 8

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
	sem    chan struct{}                   // bounds the chain checks of the background loop
	// inbox returns the stored received messages of an order (the messenger's); a stub case keeps only the
	// ids of the dispute.open messages and reads them from here once the case opens.
	inbox func(orderID string) []*nostr.Event
}

// Wait waits for the background checks that are running.
func (e *Engine) Wait() { e.jobs.Wait() }

// New creates the engine and registers its handlers.
func New(d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	e := &Engine{Deps: d, log: d.Log.With("component", "escrow"), sem: make(chan struct{}, watchParallel)}
	m := d.Messenger
	e.inbox = m.Inbox
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
	now := time.Now().Unix()
	for _, c := range cases {
		id := c.OrderID
		switch c.State {
		case CaseFeePending:
			if c.NextCheck > now {
				continue
			}
			e.limited(ctx, id, func(ctx context.Context) { e.evaluate(ctx, id) })
		case CaseOpen:
			e.limited(ctx, id, func(ctx context.Context) { e.checkSpent(ctx, id) })
		case CaseRuled:
			e.limited(ctx, id, func(ctx context.Context) { e.sendRuling(ctx, id); e.checkSpent(ctx, id) })
		}
	}
}

// limited runs fn in the background like background, at most watchParallel at once.
func (e *Engine) limited(ctx context.Context, id string, fn func(ctx context.Context)) {
	e.background(ctx, id, func(ctx context.Context) {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-e.sem }()
		fn(ctx)
	})
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

// notice is an escrow.notice received.
type notice struct {
	From     string         `json:"from"`
	Received int64          `json:"received"`
	Events   []*nostr.Event `json:"events"`
}

func (n notice) requestID() string {
	for _, ev := range n.Events {
		if ev != nil && giftwrap.Type(ev) == proto.TypeOrderRequest {
			return ev.ID
		}
	}
	return ""
}

// noticeSet is what the notices bucket holds per order: every notice naming a different request (bounded),
// since only the chain tells which of them is the real order (§4.7).
type noticeSet struct {
	Notices []notice `json:"notices,omitempty"`
	// the single notice of earlier versions
	From     string         `json:"from,omitempty"`
	Received int64          `json:"received,omitempty"`
	Events   []*nostr.Event `json:"events,omitempty"`
}

func (s *noticeSet) list() []notice {
	if len(s.Notices) == 0 && len(s.Events) > 0 {
		return []notice{{From: s.From, Received: s.Received, Events: s.Events}}
	}
	return s.Notices
}

func (e *Engine) notices(orderID string) []notice {
	var s noticeSet
	_, _ = e.DB.Get(bucketNotices, orderID, &s)
	return s.list()
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
	// a later notice of the same request may complete or correct the funding; notices of other requests are
	// kept too (a stranger may have sent one first) until the chain shows which one is funded
	rec := notice{From: msg.From, Received: time.Now().Unix(), Events: o.Events()}
	var dropped bool
	err = store.Modify(e.DB, bucketNotices, o.ID, func(s *noticeSet, _ bool) error {
		list := s.list()
		for i := range list {
			if list[i].requestID() == o.RequestEvent.ID {
				list[i] = rec
				*s = noticeSet{Notices: list}
				return nil
			}
		}
		if len(list) >= maxCandidates {
			dropped = true
			return store.ErrStop
		}
		*s = noticeSet{Notices: append(list, rec)}
		return nil
	})
	if err != nil {
		e.log.Warn("escrow.notice not recorded", "order", o.ID, "err", err)
		return
	}
	if dropped {
		e.log.Warn("escrow.notice dropped: too many requests for one order", "order", o.ID, "request", o.RequestEvent.ID)
		return
	}
	e.log.Info("escrow.notice recorded", "order", o.ID, "asset", o.Quote.Asset, "lock", o.Quote.LockAmount)
	// a case waiting for a verifiable agreement looks again
	var recheck bool
	e.mu.Lock()
	_ = store.Modify(e.DB, bucketCases, o.ID, func(c *Case, exists bool) error {
		if !exists {
			return store.ErrStop
		}
		if c.Verified {
			if c.requestID() != o.RequestEvent.ID {
				c.note("escrow.notice of %s names another request (%s) than the verified order, ignored", msg.From, o.RequestEvent.ID)
				return nil
			}
			return store.ErrStop
		}
		recheck = c.reopen()
		return nil
	})
	e.mu.Unlock()
	if recheck {
		e.background(ctx, o.ID, func(ctx context.Context) { e.evaluate(ctx, o.ID) })
	}
}

// requestID is the id of the request of the verified agreement.
func (c *Case) requestID() string {
	for _, ev := range c.Agreement {
		if ev != nil && giftwrap.Type(ev) == proto.TypeOrderRequest {
			return ev.ID
		}
	}
	return ""
}

// reopen makes an unverified case check its candidates again (a new one arrived); it tells whether a check is
// due.
func (c *Case) reopen() bool {
	switch c.State {
	case CaseNoObligation:
		if c.Verified {
			return false
		}
		c.State = CaseFeePending
		c.note("another agreement was named: checking again")
		fallthrough
	case CaseFeePending:
		c.Checks, c.NextCheck = 0, 0
		return true
	}
	return false
}

func (e *Engine) onDisputeOpen(ctx context.Context, msg *messenger.Message) {
	var d proto.DisputeOpen
	if err := msg.Decode(&d); err != nil {
		return
	}
	me := e.Keys.NostrPubHex()
	// the agreement the evidence names, if it holds together and the sender is a party of it
	var cand *candidate
	if evs, _, err := contract.Select(msg.OrderID, nil, d.Evidence.Messages); err == nil {
		if o, err := contract.FromEvents(evs); err == nil && o.ID == msg.OrderID && o.Request.Escrow == me && (msg.From == o.User || msg.From == o.Shopper) {
			cand = &candidate{RequestID: o.RequestEvent.ID, Events: o.Events()}
		}
	}
	party := cand != nil
	for _, n := range e.notices(msg.OrderID) {
		if o, err := contract.FromEvents(n.Events); err == nil && (msg.From == o.User || msg.From == o.Shopper) {
			party = true
		}
	}
	if !party {
		e.log.Warn("dispute from a stranger, for another escrow or without a verifiable order", "order", msg.OrderID, "from", msg.From)
		return
	}
	cl := claim{From: msg.From, EventID: msg.Inner.ID, Claim: d.Claim, Text: truncate(d.Text, maxText), RequestedSplit: d.RequestedSplit, At: time.Now().Unix()}
	if cand != nil {
		cl.RequestID = cand.RequestID
	}
	var check bool
	e.mu.Lock()
	err := store.Modify(e.DB, bucketCases, msg.OrderID, func(c *Case, exists bool) error {
		if !exists {
			// a stub until the funding and the fee are verified: the evidence stays in the inbox until then
			*c = Case{OrderID: msg.OrderID, State: CaseFeePending, Opened: time.Now().Unix(), OpenedBy: msg.From, Claim: d.Claim}
			c.note("dispute.open from %s: %s", msg.From, d.Claim)
		}
		for _, have := range c.Claims {
			if have.EventID == cl.EventID {
				return store.ErrStop
			}
		}
		if len(c.Claims) < maxClaims {
			c.Claims = append(c.Claims, cl)
		}
		if cand != nil {
			switch {
			case c.Verified && cand.RequestID != c.requestID():
				c.note("dispute.open of %s names another request (%s) than the verified order, ignored", msg.From, cand.RequestID)
			case !c.Verified && !c.hasCandidate(cand.RequestID) && len(c.Candidates) < maxCandidates:
				c.Candidates = append(c.Candidates, *cand)
			}
		}
		if exists && c.State != CaseFeePending && c.State != CaseNoObligation {
			// a later dispute.open never takes a case back (§4.8)
			c.note("dispute.open from %s on a %s case recorded", msg.From, c.State)
			return nil
		}
		check = c.reopen()
		return nil
	})
	e.mu.Unlock()
	if err != nil && !errors.Is(err, store.ErrStop) {
		e.log.Error("store case", "err", err)
		return
	}
	if check {
		// the checks ask the chain; they must not hold up the messages
		e.background(ctx, msg.OrderID, func(ctx context.Context) { e.evaluate(ctx, msg.OrderID) })
	}
}

func (c *Case) hasCandidate(requestID string) bool {
	for _, have := range c.Candidates {
		if have.RequestID == requestID {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// errFeeConflict: the upfront fee is counted for another verified order. It is checked again (the other
// order's case may turn out not to hold) until the case expires.
var errFeeConflict = errors.New("upfront fee counted for another order")

// evaluate decides a pending case (§4.7): it looks for the agreement whose funding the chain shows paying our
// own key for the order, then for a dispute.open of one of its parties, then for the upfront fee we are owed.
// Anything the chain could not answer yet keeps the case pending, to be checked again with a growing pause.
func (e *Engine) evaluate(ctx context.Context, id string) {
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok || c.State != CaseFeePending {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 1. the verified agreement
	var o *contract.Order
	var fund *btc.Tx
	failed := map[string]string{}
	var pending error
	if c.Verified {
		if o, err = contract.FromEvents(c.Agreement); err != nil {
			return
		}
		if o.Quote.Asset == proto.AssetBTC {
			// the fee is looked for in the funding transaction itself
			fund, err = e.verifyFunding(cctx, o)
			if err != nil {
				pending = err
				o = nil
			}
		}
	} else {
		for _, cand := range e.candidates(c) {
			if cand.Failed != "" {
				failed[cand.RequestID] = cand.Failed
				continue
			}
			co, err := contract.FromEvents(cand.Events)
			if err == nil && (co.ID != id || co.Request.Escrow != e.Keys.NostrPubHex()) {
				err = contract.Mismatchf("not an order of ours")
			}
			var tx *btc.Tx
			if err == nil {
				tx, err = e.verifyFunding(cctx, co)
			}
			switch {
			case err == nil:
				o, fund = co, tx
			case contract.IsDefinite(err):
				failed[cand.RequestID] = err.Error()
			default:
				pending = err
			}
			if o != nil {
				break
			}
		}
	}

	// 2. the dispute of a party, 3. the fee
	var opener *claim
	var feeErr error
	if o != nil {
		for i := range c.Claims {
			if cl := &c.Claims[i]; cl.From == o.User || cl.From == o.Shopper {
				opener = cl
				break
			}
		}
		if opener != nil {
			var use string
			use, feeErr = e.feeOwed(cctx, o, fund)
			if feeErr == nil {
				feeErr = e.claimFee(use, id)
			}
		}
	}

	var opened bool
	e.mu.Lock()
	_ = store.Modify(e.DB, bucketCases, id, func(c *Case, exists bool) error {
		if !exists || c.State != CaseFeePending {
			return store.ErrStop
		}
		for i := range c.Candidates {
			if why := failed[c.Candidates[i].RequestID]; why != "" && c.Candidates[i].Failed == "" {
				c.Candidates[i].Failed = why
				c.note("agreement %s does not verify: %s", c.Candidates[i].RequestID, why)
			}
		}
		if o != nil && !c.Verified {
			c.Verified, c.Agreement, c.Candidates = true, o.Events(), nil
			c.User, c.Shopper, c.Asset = o.User, o.Shopper, o.Quote.Asset
			c.note("funding of request %s verified on chain", o.RequestEvent.ID)
			for req := range failed {
				if req != o.RequestEvent.ID {
					c.note("conflicting request %s recorded, not the funded order", req)
				}
			}
		}
		switch {
		case o == nil && pending == nil:
			c.State = CaseNoObligation
			c.note("no agreement named for this order is funded to our key: no obligation to rule")
			return nil
		case o != nil && opener == nil:
			c.note("waiting for a dispute.open of a party of the verified order")
		case o != nil && feeErr == nil:
			c.FeePaid, c.State, c.OpenedBy, c.Claim, c.Text, c.RequestedSplit = true, CaseOpen, opener.From, opener.Claim, opener.Text, opener.RequestedSplit
			c.Checks, c.NextCheck = 0, 0
			c.note("upfront fee paid; case opened by %s: %s", opener.From, opener.Claim)
			opened = true
			return nil
		case o != nil && contract.IsDefinite(feeErr):
			c.State = CaseNoObligation
			c.note("upfront fee not owed (%v): no obligation to rule", feeErr)
			return nil
		case o != nil:
			c.note("upfront fee could not be checked yet: %v", feeErr)
		default:
			c.note("funding could not be checked yet: %v", pending)
		}
		if time.Since(time.Unix(c.Opened, 0)) > PendingTTL {
			c.State = CaseNoObligation
			c.note("not verified within %s: no obligation to rule", PendingTTL)
			return nil
		}
		c.Checks++
		c.NextCheck = time.Now().Add(backoff(c.Checks)).Unix()
		return nil
	})
	e.mu.Unlock()
	if opened {
		e.opened(ctx, id, o)
	}
}

// backoff is the pause before check n+1 of a pending case.
func backoff(n int) time.Duration {
	d := CheckBase
	for i := 1; i < n && d < CheckMax; i++ {
		d *= 2
	}
	return min(d, CheckMax)
}

// candidates lists the agreements named for a stub case: the notices first, then the evidence of disputes.
func (e *Engine) candidates(c *Case) []candidate {
	var out []candidate
	seen := map[string]bool{}
	failed := map[string]string{}
	for _, cand := range c.Candidates {
		failed[cand.RequestID] = cand.Failed
	}
	for _, n := range e.notices(c.OrderID) {
		if id := n.requestID(); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, candidate{RequestID: id, Events: n.Events, Failed: failed[id]})
		}
	}
	for _, cand := range c.Candidates {
		if !seen[cand.RequestID] {
			seen[cand.RequestID] = true
			out = append(out, cand)
		}
	}
	return out
}

// opened fills a case that just opened from the messages its stub left in the inbox (the dispute.open of the
// opener, evidence and attachments that came early) and asks the parties for their evidence.
func (e *Engine) opened(ctx context.Context, id string, o *contract.Order) {
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok {
		return
	}
	var have bool
	var early []*nostr.Event
	for _, ev := range e.inbox(id) {
		if ev.PubKey != o.User && ev.PubKey != o.Shopper {
			continue
		}
		switch giftwrap.Type(ev) {
		case proto.TypeDisputeOpen:
			var d proto.DisputeOpen
			if ev.ID != c.claimID(ev.PubKey) || json.Unmarshal([]byte(ev.Content), &d) != nil {
				continue
			}
			have = have || ev.PubKey == c.OpenedBy
			e.mu.Lock()
			_ = store.Modify(e.DB, bucketCases, id, func(c *Case, _ bool) error {
				c.addEvidence(ev.PubKey, ev.ID, d.Evidence)
				_, ignored, _ := contract.Select(id, c.Agreement, d.Evidence.Messages)
				for _, ig := range ignored {
					c.note("evidence message %s differs from the verified order, ignored", ig)
				}
				e.decryptAddress(c, o, d.Evidence)
				return nil
			})
			e.mu.Unlock()
		case proto.TypeDisputeEvidence, proto.TypeAttachment:
			early = append(early, ev)
		}
	}
	for _, ev := range early {
		m := &messenger.Message{Inner: ev, From: ev.PubKey, Type: giftwrap.Type(ev), OrderID: id}
		if m.Type == proto.TypeAttachment {
			e.onAttachment(ctx, m)
		} else {
			e.onEvidence(ctx, m)
		}
	}
	want := proto.EvidenceRequest{Want: []string{"messages", "tracking", "purchase_evidence", "delivery_key_for_escrow"}}
	to := []string{o.Shopper}
	if c.OpenedBy == o.Shopper {
		to = []string{o.User}
	}
	if !have {
		to = append(to, c.OpenedBy) // its dispute.open is no longer at hand
	}
	for _, p := range to {
		if _, err := e.Messenger.Send(ctx, p, o.ID, proto.TypeDisputeEvidenceRequest, want, o.Request.Relays); err != nil {
			e.log.Warn("evidence request not sent", "order", o.ID, "to", p, "err", err)
		}
	}
}

// claimID is the id of the first dispute.open of a party.
func (c *Case) claimID(from string) string {
	for _, cl := range c.Claims {
		if cl.From == from {
			return cl.EventID
		}
	}
	return ""
}

// addEvidence appends the evidence of a party once per message id, within the bound.
func (c *Case) addEvidence(from, id string, ev proto.DisputeEvidence) bool {
	if slices.Contains(c.EvidenceIDs, id) {
		return false
	}
	if c.Evidence == nil {
		c.Evidence = map[string][]proto.DisputeEvidence{}
	}
	if len(c.Evidence[from]) >= maxEvidencePerParty {
		c.note("evidence of %s beyond %d messages, ignored", from, maxEvidencePerParty)
		return false
	}
	c.EvidenceIDs = append(c.EvidenceIDs, id)
	c.Evidence[from] = append(c.Evidence[from], ev)
	c.note("evidence from %s: %d messages, %d tracking entries", from, len(ev.Messages), len(ev.Tracking))
	return true
}

// claimFee records that a fee pays for this order. A fee counted for another order whose case holds (verified,
// fee paid) does not count; one counted for a case that no longer holds is taken over.
func (e *Engine) claimFee(use, id string) error {
	var owner string
	if ok, _ := e.DB.Get(bucketFeeUses, use, &owner); ok && owner != id {
		if oc, ok, _ := e.caseDoc(owner); ok && oc.Verified && oc.FeePaid && oc.State != CaseNoObligation {
			return fmt.Errorf("%w: %s already paid for order %s", errFeeConflict, use, owner)
		}
	}
	return store.Modify(e.DB, bucketFeeUses, use, func(cur *string, exists bool) error {
		if exists && *cur == id {
			return store.ErrStop
		}
		if exists && *cur != owner {
			return fmt.Errorf("%w: %s was just claimed for order %s", errFeeConflict, use, *cur)
		}
		*cur = id
		return nil
	})
}

// ID is the key of the case.
func (c *Case) ID() string { return c.OrderID }

// verifyFunding checks on chain that the funding of an agreement pays the contract rebuilt from its request and
// quote with our own key for the order (§4.7): BTC, the P2WSH output with the lock amount, confirmed (the
// funding transaction is returned, for the fee); USDC, the predicted Safe with the owners, threshold and module
// of the order, holding the lock amount. Definite errors mean the agreement is not the funded order.
func (e *Engine) verifyFunding(ctx context.Context, o *contract.Order) (*btc.Tx, error) {
	if o.Funded == nil {
		return nil, contract.Mismatchf("no order.funded")
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(o.Request, o.Quote)
		if err != nil {
			return nil, contract.Definite(err)
		}
		key, err := e.Keys.EscrowOrderKey(o.ID)
		if err != nil {
			return nil, err
		}
		if !key.PubKey().IsEqual(esc.Escrow) {
			return nil, contract.Mismatchf("the quote names another escrow key than ours for the order")
		}
		if e.BTC == nil {
			return nil, errors.New("no bitcoin backend")
		}
		tx, _, err := contract.BTCEscrowOutput(ctx, e.BTC, esc, o.Quote, o.Funded, 1)
		return tx, err
	case proto.AssetUSDC:
		if e.EVM == nil || e.Deployments == nil {
			return nil, errors.New("no EVM backend")
		}
		if !common.IsHexAddress(o.Quote.EscrowEVMAddress) || common.HexToAddress(o.Quote.EscrowEVMAddress) != e.Keys.EVMAddress() {
			return nil, contract.Mismatchf("the quote names another escrow address than ours")
		}
		if !common.IsHexAddress(o.Funded.Safe) {
			return nil, contract.Mismatchf("funded without a safe address")
		}
		d, err := e.Deployments()
		if err != nil {
			return nil, err
		}
		os, err := contract.SafeOf(d, o.Request, o.Quote)
		if err != nil {
			return nil, contract.Definite(err)
		}
		safe := common.HexToAddress(o.Funded.Safe)
		if err := contract.CheckSafe(ctx, e.EVM, d, os, o.ID, o.Quote, safe); err != nil {
			return nil, err
		}
		lock, err := amount.ParseInt(o.Quote.LockAmount)
		if err != nil {
			return nil, contract.Definite(err)
		}
		bal, err := e.EVM.BalanceOf(ctx, d.USDC, safe)
		if err != nil {
			return nil, err
		}
		if bal.Cmp(lock) < 0 {
			return nil, contract.ErrNotYet
		}
		return nil, nil
	}
	return nil, contract.Mismatchf("unknown asset %q", o.Quote.Asset)
}

// feeOwed checks that we are owed a ruling (§4.7): the quote's upfront fee is above 0 and at least our own
// max(bps × lock, min), and it was paid as §4.6 says (BTC: in the verified funding transaction fund). It returns
// the key of the fee for claimFee. Definite errors mean no obligation.
func (e *Engine) feeOwed(ctx context.Context, o *contract.Order, fund *btc.Tx) (string, error) {
	fee, err := amount.ParseInt(o.Quote.EscrowUpfrontFee)
	if err != nil {
		return "", contract.Mismatchf("escrow_upfront_fee: %v", err)
	}
	if fee.Sign() <= 0 {
		return "", contract.Mismatchf("no upfront fee was quoted")
	}
	lock, err := amount.ParseInt(o.Quote.LockAmount)
	if err != nil {
		return "", contract.Mismatchf("lock_amount: %v", err)
	}
	cfg := e.Config.UpfrontFee
	due := amount.BPS(lock, cfg.BPS)
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if cfg.MinSats != "" {
			min, err := amount.ParseInt(cfg.MinSats)
			if err != nil {
				return "", fmt.Errorf("config min_sats: %w", err)
			}
			due = amount.Max(due, min)
		}
	case proto.AssetUSDC:
		if cfg.MinUSDC != "" {
			min, err := amount.Parse(cfg.MinUSDC)
			if err != nil {
				return "", fmt.Errorf("config min_usdc: %w", err)
			}
			due = amount.Max(due, amount.ToUnits(min, 6))
		}
	}
	if fee.Cmp(due) < 0 {
		return "", contract.Mismatchf("the quoted upfront fee %s is below our fee %s", fee, due)
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if o.Quote.EscrowBTCFeeAddress != e.Keys.WalletAddress() {
			return "", contract.Mismatchf("the fee was quoted to another address")
		}
		if o.Funded.FeeTxID != "" && o.Funded.FeeTxID != o.Funded.TxID {
			return "", contract.Mismatchf("the upfront fee was not paid in the funding transaction")
		}
		if fund == nil || fund.TxID != o.Funded.TxID {
			return "", errors.New("funding transaction not at hand")
		}
		if err := contract.BTCFeePaid(fund, o.Quote); err != nil {
			return "", err
		}
		return "btc-fee:" + fund.TxID, nil
	case proto.AssetUSDC:
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
		// a stub keeps nothing: the message stays in the inbox and is taken when the case opens
		if !exists || !c.Verified || c.State == CaseFeePending || c.State == CaseNoObligation || (msg.From != c.User && msg.From != c.Shopper) {
			return store.ErrStop
		}
		if slices.Contains(c.EvidenceIDs, msg.Inner.ID) {
			return store.ErrStop // a resent message counts once
		}
		for _, m := range ev.Messages {
			if m == nil || giftwrap.VerifyInner(m) != nil {
				c.note("evidence of %s contains an invalid message, ignored", msg.From)
				return nil
			}
		}
		if !c.addEvidence(msg.From, msg.Inner.ID, ev) {
			return nil
		}
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
	if err != nil || !ok || !c.Verified || c.State == CaseFeePending || c.State == CaseNoObligation || (msg.From != c.User && msg.From != c.Shopper) {
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
	if msg.Decode(&ref) != nil || (!contract.IsTxID(ref.TxID) && !contract.IsTxHash(ref.TxID)) {
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
		lock, err := amount.ParseInt(o.Quote.LockAmount)
		if err != nil {
			return
		}
		// paid out: the balance fell below the lock by a transfer out of the Safe (§4.8; a dust transfer into
		// the Safe may stay behind)
		by, err = contract.SafeSettlement(cctx, e.EVM, d, common.HexToAddress(o.Funded.Safe), lock, c.ClaimedPayout)
		if err != nil || by == "" {
			return
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
		e.evaluate(ctx, id)
		if c, _, err = e.caseDoc(id); err != nil {
			return nil, err
		}
	}
	switch c.State {
	case CaseNoObligation:
		return nil, ErrNoObligation
	case CaseFeePending:
		return nil, errors.New("the funding or the upfront fee could not be verified yet; try again")
	case CaseRuled:
		return nil, ErrAlreadyRuled
	case CaseClosed:
		if c.Ruling != nil {
			return nil, ErrAlreadyRuled
		}
		return nil, errors.New("case is closed")
	}
	if !c.Verified {
		return nil, errors.New("case without a verified order")
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
	e.log.Info("ruled", "order", id, "user", ruling.Split.User, "shopper", ruling.Split.Shopper, "fee", ruling.Split.EscrowFee)
	// each party gets it on its own; one that could not be sent is sent again by the background checks
	e.sendRuling(ctx, id)
	return ruling, nil
}

// sendRuling sends the recorded ruling of a case to the parties that did not get it yet.
func (e *Engine) sendRuling(ctx context.Context, id string) {
	c, ok, err := e.caseDoc(id)
	if err != nil || !ok || c.Ruling == nil {
		return
	}
	o, err := contract.FromEvents(c.Agreement)
	if err != nil {
		return
	}
	for _, to := range []string{o.User, o.Shopper} {
		if c.RulingSent[to] != "" {
			continue
		}
		ev, err := e.Messenger.Send(ctx, to, o.ID, proto.TypeDisputeRuling, c.Ruling, o.Request.Relays)
		e.mu.Lock()
		_ = store.Modify(e.DB, bucketCases, id, func(c *Case, _ bool) error {
			if err != nil {
				c.note("ruling not sent to %s, will retry: %v", to, err)
				return nil
			}
			if c.RulingSent == nil {
				c.RulingSent = map[string]string{}
			}
			c.RulingSent[to] = ev.ID
			c.note("ruling sent to %s", to)
			return nil
		})
		e.mu.Unlock()
		if err != nil {
			e.log.Warn("ruling not sent", "order", id, "to", to, "err", err)
		}
	}
}

// split checks user + shopper against total minus our dispute fee and returns the fee. The fee is
// dispute_fee_bps of total, which is also its cap (§4.8); a fee below dust is not taken at all, so that the
// payout creates no output that would not relay.
func (e *Engine) split(total, user, shopper *big.Int, dust int64) (*big.Int, error) {
	if total.Sign() <= 0 || user.Sign() < 0 || shopper.Sign() < 0 {
		return nil, fmt.Errorf("nothing to divide (%s)", total)
	}
	fee := amount.BPS(total, e.Config.DisputeFeeBPS)
	if fee.Cmp(big.NewInt(dust)) < 0 {
		fee = new(big.Int)
	}
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
	quoted, err := amount.ParseInt(o.Quote.PayoutFeeReserve)
	if err != nil {
		return nil, err
	}
	lock, err := amount.ParseInt(o.Quote.LockAmount)
	if err != nil {
		return nil, err
	}
	// the reserve is the miner fee of the payout; no more than user clients accept for the lock (§4.5)
	reserve := min(quoted.Int64(), contract.ReserveCap(lock.Int64()))
	if reserve < 0 || reserve >= prev.Amount {
		return nil, fmt.Errorf("payout fee reserve %d does not fit the escrow of %d sats", reserve, prev.Amount)
	}
	total := big.NewInt(prev.Amount - reserve)
	fee, err := e.split(total, user, shopper, btc.DustLimit)
	if err != nil {
		return nil, err
	}
	for name, v := range map[string]*big.Int{"user": user, "shopper": shopper} {
		if v.Sign() > 0 && v.Int64() < btc.DustLimit {
			return nil, fmt.Errorf("the %s share of %s sats is dust (below %d): give it 0 or at least %d", name, v, btc.DustLimit, btc.DustLimit)
		}
	}
	p, err := esc.NewSpend(prev, []btc.Output{
		{Address: o.Request.UserBTCAddress, Amount: user.Int64()},
		{Address: o.Quote.ShopperBTCAddress, Amount: shopper.Int64()},
		{Address: e.Keys.WalletAddress(), Amount: fee.Int64()},
	}, btc.PathMultisig)
	if err != nil {
		return nil, err
	}
	script, err := esc.Script()
	if err != nil {
		return nil, err
	}
	if need := btc.MultisigVSize(p.UnsignedTx, script) * btc.MinRelayFeeRate; reserve < need {
		return nil, fmt.Errorf("payout fee reserve %d sats is below the minimum relay fee %d of the payout", reserve, need)
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
	// the split divides the balance at signing (§4.8): anyone can send the Safe a little more at any time
	bal, err := e.EVM.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return nil, err
	}
	fee, err := e.split(bal, user, shopper, 0)
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
