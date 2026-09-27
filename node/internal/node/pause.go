package node

import (
	"context"
	"errors"
	"time"
)

// MaxPause bounds POST /admin/pause.
const MaxPause = 24 * time.Hour

// startActiveLocked starts the messenger (subscriptions, sending, resending, acks) and the tick loops of the role
// engine under a context that Pause ends. n.pauseMu is held.
func (n *Node) startActiveLocked() {
	actx, cancel := context.WithCancel(n.runCtx)
	n.stopActive = cancel
	if n.msgr != nil {
		n.msgr.SetPaused(false)
		n.msgr.Start(actx)
	}
	if n.shopper != nil {
		n.shopper.Start(actx)
	}
	if n.escrow != nil {
		n.escrow.Start(actx)
	}
}

// Pause stops the node's message traffic for d: the messenger's subscriptions, sending (kept in the outbox) and
// resending, and the engines' tick loops (funding checks, tracking, payouts, T1 claims). libp2p, the trust
// bridge and the admin API keep running. Pausing again extends or shortens the pause. It returns when it ends.
// The lab uses it to make a shopper "disappear" (e2e scenario h) without access to docker.
func (n *Node) Pause(d time.Duration) (time.Time, error) {
	if d <= 0 || d > MaxPause {
		return time.Time{}, errors.New("pause must be between 1 second and 24 hours")
	}
	n.pauseMu.Lock()
	defer n.pauseMu.Unlock()
	if n.runCtx == nil {
		return time.Time{}, errors.New("node not running")
	}
	if n.msgr != nil {
		n.msgr.SetPaused(true)
	}
	if n.stopActive != nil {
		n.stopActive()
		n.stopActive = nil
	}
	if n.resumeTimer != nil {
		n.resumeTimer.Stop()
	}
	n.pausedUntil = time.Now().Add(d)
	n.resumeTimer = time.AfterFunc(d, func() { n.Resume() })
	n.log.Warn("network activity paused", "until", n.pausedUntil.Format(time.RFC3339))
	return n.pausedUntil, nil
}

// Resume ends a pause at once; it reports whether the node was paused.
func (n *Node) Resume() bool {
	n.pauseMu.Lock()
	defer n.pauseMu.Unlock()
	if n.pausedUntil.IsZero() || n.runCtx == nil || n.runCtx.Err() != nil {
		return false
	}
	if n.resumeTimer != nil {
		n.resumeTimer.Stop()
		n.resumeTimer = nil
	}
	n.pausedUntil = time.Time{}
	n.startActiveLocked()
	n.log.Warn("network activity resumed")
	return true
}

// PausedUntil is the end of the current pause (zero when not paused).
func (n *Node) PausedUntil() time.Time {
	n.pauseMu.Lock()
	defer n.pauseMu.Unlock()
	return n.pausedUntil
}
