package node

import (
	"context"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// BundleInterval is how often a node fetches its bundles again (§2.6: at start and every 10 minutes).
var BundleInterval = 10 * time.Minute

// MaxListURLsPerRound bounds the list_url bundles fetched in one round.
var MaxListURLsPerRound = 64

// bundleLoop implements the fetch order of §2.6: at start and every BundleInterval the trust bundles
// (trust.bundle_urls), then the list bundles of the effective delegations (list_url), then trust-sync with the
// connected peers (Nostr, when trust.nostr is on, runs as its own subscription). A list_url that appears between
// two rounds (a delegation came by gossip) is fetched when it appears. Every event goes through the same checks as
// any other (trust.Store.Put: signature, version, network, scope), and new ones are gossiped.
func (n *Node) bundleLoop(ctx context.Context) {
	changes := n.trust.Watch()
	t := time.NewTicker(BundleInterval)
	defer t.Stop()
	for round := 0; ; round++ {
		done := map[string]bool{}
		for _, u := range n.cfg.Trust.BundleURLs {
			n.fetchBundle(ctx, u)
		}
		n.fetchListBundles(ctx, done)
		if round > 0 {
			// at start, every new connection syncs anyway
			n.p2p.SyncAll(ctx)
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				break wait
			case <-changes:
				select { // a delegation and its list tend to come together
				case <-ctx.Done():
					return
				case <-time.After(n.debounce):
				}
				n.fetchListBundles(ctx, done)
			}
		}
	}
}

// fetchListBundles fetches the list_url bundles of the effective delegations that are not in done, until no new
// one appears (a bundle may carry further delegations).
func (n *Node) fetchListBundles(ctx context.Context, done map[string]bool) {
	if n.cfg.Role == config.RoleRelay {
		return // a p2p relay has no coordinators of its own (§10)
	}
	for {
		var todo []string
		for _, u := range n.trust.ListURLs(n.cfg.Trust.Coordinators, n.cfg.Network) {
			if !done[u] && len(done)+len(todo) < MaxListURLsPerRound {
				todo = append(todo, u)
			}
		}
		if len(todo) == 0 || ctx.Err() != nil {
			return
		}
		for _, u := range todo {
			done[u] = true
			n.fetchBundle(ctx, u)
		}
	}
}

func (n *Node) fetchBundle(ctx context.Context, url string) {
	fctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	evs, err := trust.FetchBundle(fctx, n.http, url)
	if err != nil {
		n.log.Warn("bundle", "url", url, "err", err)
		return
	}
	stored, rejected := n.trust.PutBundle(evs, n.cfg.Network)
	n.log.Info("bundle", "url", url, "events", len(evs), "new", len(stored), "rejected", rejected)
	for _, ev := range stored {
		if err := n.p2p.Publish(ctx, ev); err != nil {
			n.log.Debug("gossip failed", "err", err)
		}
	}
}
