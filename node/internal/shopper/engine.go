// Package shopper is the shopper side of the order flow (spec §11): it quotes requests after the trust, region,
// payment and risk checks, verifies the funding, buys through shopper-bot, reports the shipping, countersigns
// releases and rulings, answers the escrow's evidence requests, and claims through the T1 branch when the user
// never releases a delivered order.
package shopper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/botclient"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/fx"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shop"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

var (
	errUnknownOrder = errors.New("unknown order")
	errSkip         = errors.New("nothing to change")
)

// BTCChain is the Esplora subset the shopper uses (*btc.Esplora implements it).
type BTCChain interface {
	TipHeight(ctx context.Context) (int64, error)
	Tx(ctx context.Context, txid string) (*btc.Tx, error)
	Confirmations(ctx context.Context, txid string) (int64, error)
	Outspend(ctx context.Context, txid string, vout uint32) (*btc.Outspend, error)
	Broadcast(ctx context.Context, txHex string) (string, error)
}

// Deps are the collaborators of the engine. BTC and EVM may be nil when the chain is not configured.
type Deps struct {
	Keys         *keys.Set
	Messenger    *messenger.Messenger
	Trust        *trust.Store
	Coordinators []string
	Network      string
	FX           *fx.Service
	BTC          BTCChain
	EVM          *evm.Client
	Deployments  func() (*evm.Deployments, error)
	Bot          *botclient.Client
	Shops        *shop.Inspector
	DB           *store.DB
	Config       *config.Shopper
	Name         string
	Log          *slog.Logger
}

// Engine runs the shopper side.
type Engine struct {
	Deps
	db  *store.DB
	cfg *config.Shopper
	log *slog.Logger

	locks sync.Map // order id → *sync.Mutex, serializes the work on one order
	busy  sync.Map // "<job>:<order id>" → struct{}, background jobs in progress (at most one per job and order)
	jobs  sync.WaitGroup
	base  atomic.Pointer[context.Context] // the context of Start, for background jobs
}

// Wait waits for the background jobs that are running.
func (e *Engine) Wait() { e.jobs.Wait() }

// background runs fn in its own goroutine unless the same job for the order is still running. Message handlers
// use it for anything that talks to a chain, the bot or a shop, so that one slow order does not hold up the
// messages of all others. fn gets the engine's context: the context of a message handler ends with the handler.
func (e *Engine) background(ctx context.Context, job, id string, fn func(ctx context.Context)) {
	key := job + ":" + id
	if _, running := e.busy.LoadOrStore(key, struct{}{}); running {
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
		defer e.busy.Delete(key)
		fn(ctx)
	}()
}

// New creates the engine and registers its message handlers.
func New(d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	e := &Engine{Deps: d, db: d.DB, cfg: d.Config, log: d.Log.With("component", "shopper")}
	if d.Shops != nil {
		d.Shops.AllowPrivate = d.Config.AllowPrivateShops
	}
	m := d.Messenger
	m.Handle(proto.TypeOrderRequest, e.onRequest)
	m.Handle(proto.TypeOrderEscrowKey, e.onEscrowKey)
	m.Handle(proto.TypeOrderAccept, e.onAccept)
	m.Handle(proto.TypeOrderCancel, e.onCancel)
	m.Handle(proto.TypeOrderFunded, e.onFunded)
	m.Handle(proto.TypeOrderRelease, e.onRelease)
	m.Handle(proto.TypeDisputeOpen, e.onDisputeOpen)
	m.Handle(proto.TypeDisputeEvidenceRequest, e.onEvidenceRequest)
	m.Handle(proto.TypeDisputeRuling, e.onRuling)
	m.Handle(proto.TypeDisputeCountersigned, e.onCountersigned)
	m.HandleOther(func(_ context.Context, msg *messenger.Message) {
		e.log.Info("message", "type", msg.Type, "from", msg.From, "order", msg.OrderID, "content", msg.Inner.Content)
	})
	return e
}

// Start runs the background loops until ctx ends.
func (e *Engine) Start(ctx context.Context) {
	e.base.Store(&ctx)
	go e.loop(ctx, 2*time.Second, e.checkFunding)
	go e.loop(ctx, time.Duration(e.cfg.TrackingPollSeconds)*time.Second, e.pollTracking)
	go e.loop(ctx, 5*time.Second, e.watchEscrows)
	go e.loop(ctx, 3*time.Second, e.retryPending)
}

func (e *Engine) loop(ctx context.Context, every time.Duration, fn func(ctx context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) lock(id string) func() {
	v, _ := e.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Profile is the content of our kind 30502.
func (e *Engine) Profile(p2p *trust.P2PInfo) trust.ShopperProfile {
	fee := map[string]any{"bps": e.cfg.Fee.BPS, "min": e.cfg.Fee.Min}
	return trust.ShopperProfile{
		Name: e.Name, Payments: e.cfg.Payments, Currencies: e.cfg.Currencies, CashRegions: e.cfg.CashRegions,
		Fee: mustJSON(fee), MaxOrder: mustJSON(e.cfg.MaxOrder), DeliveryDays: e.cfg.DeliveryDays,
		EVMAddress: e.Keys.EVMAddress().Hex(), BTCAddress: e.Keys.WalletAddress(), P2P: p2p,
	}
}

// send delivers a message about an order to the user (or another party) using the relays the user named.
func (e *Engine) send(ctx context.Context, o *Order, to, typ string, body any) (*nostr.Event, error) {
	ev, err := e.Messenger.Send(ctx, to, o.ID, typ, body, o.Relays)
	if err != nil {
		return nil, fmt.Errorf("send %s: %w", typ, err)
	}
	return ev, nil
}

func (e *Engine) fail(id, detail string, err error) {
	e.log.Warn(detail, "order", id, "err", err)
	_, _ = e.update(id, func(o *Order) error {
		o.Error = fmt.Sprintf("%s: %v", detail, err)
		o.note(o.Error)
		return nil
	})
}
