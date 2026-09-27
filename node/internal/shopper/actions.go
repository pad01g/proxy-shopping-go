package shopper

import (
	"context"
	"errors"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// maxAttempts bounds the retries of an action that keeps failing the same transient way.
const maxAttempts = 50

// actionOrder is the order in which pending actions of one order are worked off.
var actionOrder = []string{ActQuote, ActRefund, ActRelease, ActRuling, ActClaim}

// kick works off the pending actions of an order in the background (one worker per order).
func (e *Engine) kick(ctx context.Context, id string) {
	e.background(ctx, "work", id, func(ctx context.Context) { e.work(ctx, id) })
}

// retryPending restarts the workers of orders with pending actions (after a failure or a restart).
func (e *Engine) retryPending(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	for _, o := range orders {
		if len(o.Pending) > 0 {
			e.kick(ctx, o.ID)
		}
	}
}

func (e *Engine) work(ctx context.Context, id string) {
	defer e.lock(id)()
	for _, kind := range actionOrder {
		o, ok, err := e.Order(id)
		if err != nil || !ok {
			return
		}
		a := o.Pending[kind]
		if a == nil {
			continue
		}
		actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		err = e.run(actx, o, kind, a)
		cancel()
		if ctx.Err() != nil {
			return // shutting down; the action stays pending
		}
		e.settle(ctx, o, kind, err)
	}
}

// run carries out one action; nil means done, a definite error means it can never succeed.
func (e *Engine) run(ctx context.Context, o *Order, kind string, a *Action) error {
	switch kind {
	case ActQuote:
		return e.doQuote(ctx, o)
	case ActRefund:
		return e.offerRefund(ctx, o)
	}
	if a.Tx != "" {
		done, err := e.sentTx(ctx, o, kind, a)
		if err != nil {
			return err
		}
		if done {
			e.finish(ctx, o, kind, a, a.Tx)
			return nil
		}
	}
	if !o.funded() {
		return contract.Mismatchf("the escrow is no longer open")
	}
	var p *payout
	var err error
	switch kind {
	case ActRelease:
		p, err = e.prepareRelease(ctx, o, a)
	case ActRuling:
		p, err = e.prepareRuling(ctx, o, a)
	case ActClaim:
		p, err = e.prepareClaim(ctx, o)
	default:
		return contract.Mismatchf("unknown action %q", kind)
	}
	if err != nil {
		return err
	}
	txid, err := e.submit(ctx, o, kind, p)
	if err != nil {
		return err
	}
	e.finish(ctx, o, kind, a, txid)
	return nil
}

// finish records a payout of ours that reached the chain and tells the parties.
func (e *Engine) finish(ctx context.Context, o *Order, kind string, a *Action, txid string) {
	switch kind {
	case ActClaim:
		e.claimed(ctx, o.ID, txid)
		return
	case ActRelease:
		o, err := e.update(o.ID, func(o *Order) error {
			if o.PayoutTx != "" {
				return errSkip
			}
			o.Events["release"] = a.Event
			o.PayoutTx, o.PayoutBy = txid, "release"
			o.set(StateCompleted, txid)
			return nil
		})
		if err != nil {
			return
		}
		if _, err := e.send(ctx, o, o.User, proto.TypeOrderCompleted, proto.TxRef{TxID: txid}); err != nil {
			e.fail(o.ID, "completed not sent", err)
		}
		e.log.Info("released", "order", o.ID, "tx", txid)
	case ActRuling:
		o, err := e.update(o.ID, func(o *Order) error {
			if o.PayoutTx != "" {
				return errSkip
			}
			o.PayoutTx, o.PayoutBy = txid, "ruling"
			o.set(StateSettled, txid)
			return nil
		})
		if err != nil {
			return
		}
		for _, to := range []string{o.User, o.Request.Escrow} {
			if _, err := e.send(ctx, o, to, proto.TypeDisputeCountersigned, proto.TxRef{TxID: txid}); err != nil {
				e.fail(o.ID, "countersigned not sent", err)
			}
		}
		e.log.Info("ruling countersigned", "order", o.ID, "tx", txid)
	}
}

// settle removes a finished action or records why it has to be tried again.
func (e *Engine) settle(ctx context.Context, o *Order, kind string, err error) {
	gaveUp := false
	_, _ = e.update(o.ID, func(o *Order) error {
		a := o.Pending[kind]
		if a == nil {
			return errSkip
		}
		switch {
		case err == nil:
			delete(o.Pending, kind)
		case contract.IsDefinite(err) || a.Attempts+1 >= maxAttempts:
			delete(o.Pending, kind)
			o.Error = kind + ": " + err.Error()
			o.note(kind + " abandoned: " + err.Error())
			gaveUp = true
		default:
			a.Attempts++
			a.Error = err.Error()
			if !errors.Is(err, errWaiting) {
				o.note(kind + " failed, will retry: " + err.Error())
			}
		}
		return nil
	})
	if gaveUp {
		e.log.Warn(kind+" abandoned", "order", o.ID, "err", err)
		if kind == ActRelease {
			_, _ = e.send(ctx, o, o.User, proto.TypeChat, proto.Chat{Text: "release rejected: " + err.Error()})
		}
	}
}
