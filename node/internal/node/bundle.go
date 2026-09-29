package node

import (
	"context"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// BundleInterval is how often a node fetches its trust bundles (trust.bundle_urls) again.
var BundleInterval = 10 * time.Minute

// bundleLoop fetches the trust bundles at start and every BundleInterval. Their events go through the same checks
// as relay events (trust.Store.Put: signature, version, network, scope), and new ones are gossiped.
func (n *Node) bundleLoop(ctx context.Context) {
	if len(n.cfg.Trust.BundleURLs) == 0 {
		return
	}
	t := time.NewTicker(BundleInterval)
	defer t.Stop()
	for {
		for _, u := range n.cfg.Trust.BundleURLs {
			n.fetchBundle(ctx, u)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *Node) fetchBundle(ctx context.Context, url string) {
	fctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	evs, err := trust.FetchBundle(fctx, n.http, url)
	if err != nil {
		n.log.Warn("trust bundle", "url", url, "err", err)
		return
	}
	stored, rejected := n.trust.PutBundle(evs, n.cfg.Network)
	n.log.Info("trust bundle", "url", url, "events", len(evs), "new", len(stored), "rejected", rejected)
	for _, ev := range stored {
		if err := n.p2p.Publish(ctx, ev); err != nil {
			n.log.Debug("gossip failed", "err", err)
		}
	}
}
