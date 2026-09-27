package shopper

import (
	"context"
	"errors"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// Pacing of the retries of a pending action that fails for a reason that may go away: the first after
// retryBase, doubling up to retryMax. An action is given up after actionTTL, unless it sent a transaction:
// that one is watched until the chain decides.
var (
	retryBase = 3 * time.Second
	retryMax  = 10 * time.Minute
	actionTTL = 7 * 24 * time.Hour
	// waitPoll is how often a sent transaction is looked at (waiting is not an attempt).
	waitPoll = 10 * time.Second
)

// actionOrder is the order in which pending actions of one order are worked off.
var actionOrder = []string{ActQuote, ActRefund, ActRelease, ActRuling, ActClaim}

// kick works off the pending actions of an order in the background (one worker per order).
func (e *Engine) kick(ctx context.Context, id string) {
	e.background(ctx, "work", id, func(ctx context.Context) { e.work(ctx, id) })
}

// retryPending restarts the workers of orders with pending actions that are due (after a failure or a restart).
func (e *Engine) retryPending(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, o := range orders {
		for _, a := range o.Pending {
			if a.Next <= now {
				e.kick(ctx, o.ID)
				break
			}
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
		if a == nil || a.Next > time.Now().Unix() {
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
		st, err := e.sentTx(ctx, o, kind, a)
		switch {
		case err != nil:
			return err
		case st == txFinal:
			e.finish(ctx, o, kind, a, a.Tx)
			return nil
		case st == txSeen:
			// on its way: the order is paid out, the transaction is watched until it is final
			e.finish(ctx, o, kind, a, a.Tx)
			return errWaiting
		}
		// txGone: prepared and sent anew
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
	if p.tx != nil {
		// BTC: watched until it has its confirmations
		a.Tx = txid
		if st, err := e.sentTx(ctx, o, kind, a); err != nil || st != txFinal {
			return errWaiting
		}
	}
	return nil
}

// finish records a payout of ours that reached the chain and tells the parties (once).
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

// backoffAfter is the pause after attempt n of an action.
func backoffAfter(n int) time.Duration {
	d := retryBase
	for i := 1; i < n && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// settle removes a finished action or records why it has to be tried again, and when.
func (e *Engine) settle(ctx context.Context, o *Order, kind string, err error) {
	gaveUp := false
	_, _ = e.update(o.ID, func(o *Order) error {
		a := o.Pending[kind]
		if a == nil {
			return errSkip
		}
		now := time.Now()
		switch {
		case err == nil:
			delete(o.Pending, kind)
		case errors.Is(err, errWaiting):
			// a sent transaction on its way is not a failed attempt
			a.Error, a.Next = err.Error(), now.Add(waitPoll).Unix()
		case a.Tx != "" && !errors.Is(err, errSuperseded):
			// never forget a transaction we sent while the chain has not decided about it
			a.Attempts++
			a.Error, a.Next = err.Error(), now.Add(backoffAfter(a.Attempts)).Unix()
			o.note(kind + " transaction " + a.Tx + ": " + err.Error())
		case contract.IsDefinite(err) || now.Sub(time.Unix(a.Since, 0)) > actionTTL:
			delete(o.Pending, kind)
			o.Error = kind + ": " + err.Error()
			o.note(kind + " abandoned: " + err.Error())
			gaveUp = true
		default:
			a.Attempts++
			a.Error, a.Next = err.Error(), now.Add(backoffAfter(a.Attempts)).Unix()
			o.note(kind + " failed, will retry: " + err.Error())
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
