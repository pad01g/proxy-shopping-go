package node

import (
	"container/list"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/p2p"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// ContactCacheSize bounds the reply_p2p addresses remembered from order requests (users are outside the trust
// scope, like their inbox relays).
var ContactCacheSize = 1000

// contactCache remembers the latest valid reply_p2p of a pubkey (LRU).
type contactCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List
}

type cachedContact struct {
	pubkey  string
	contact proto.P2PContact
}

func newContactCache() *contactCache {
	return &contactCache{entries: map[string]*list.Element{}, lru: list.New()}
}

func (c *contactCache) put(pubkey string, contact proto.P2PContact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[pubkey]; ok {
		el.Value = &cachedContact{pubkey, contact}
		c.lru.MoveToFront(el)
		return
	}
	c.entries[pubkey] = c.lru.PushFront(&cachedContact{pubkey, contact})
	for c.lru.Len() > max(ContactCacheSize, 1) {
		last := c.lru.Back()
		c.lru.Remove(last)
		delete(c.entries, last.Value.(*cachedContact).pubkey)
	}
}

func (c *contactCache) get(pubkey string) *proto.P2PContact {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[pubkey]; ok {
		c.lru.MoveToFront(el)
		contact := el.Value.(*cachedContact).contact
		return &contact
	}
	return nil
}

// observe sees every newly stored message: the reply_p2p of an order request is remembered, so that even the
// ack of the request goes over libp2p (§4.2, §4.4).
func (n *Node) observe(msg *messenger.Message) {
	if msg.Type != proto.TypeOrderRequest {
		return
	}
	var r proto.OrderRequest
	if msg.Decode(&r) != nil || r.ReplyP2P == nil || r.ReplyP2P.Validate() != nil {
		return
	}
	n.contacts.put(msg.From, *r.ReplyP2P)
}

// contactOf returns the libp2p address of a recipient (§4.2): the p2p of its shopper or escrow profile, else the
// reply_p2p of its order request (remembered, or from the order or case we keep). nil: none known.
func (n *Node) contactOf(to, orderID string) *proto.P2PContact {
	fromProfile := func(info *trust.P2PInfo) *proto.P2PContact {
		if info == nil || info.PeerID == "" {
			return nil
		}
		return &proto.P2PContact{PeerID: info.PeerID, Addrs: info.Addrs}
	}
	if p, _ := n.trust.ShopperProfile(to, n.cfg.Network); p != nil {
		if c := fromProfile(p.P2P); c != nil {
			return c
		}
	}
	if p, _ := n.trust.EscrowProfile(to, n.cfg.Network); p != nil {
		if c := fromProfile(p.P2P); c != nil {
			return c
		}
	}
	if c := n.contacts.get(to); c != nil {
		return c
	}
	if orderID == "" {
		return nil
	}
	switch {
	case n.shopper != nil:
		if o, ok, err := n.shopper.Order(orderID); err == nil && ok && o.User == to && o.ReplyP2P != nil {
			return o.ReplyP2P
		}
	case n.escrow != nil:
		if c, ok, err := n.escrow.Case(orderID); err == nil && ok && c.User == to {
			for _, ev := range c.Agreement {
				if ev == nil || giftwrap.Type(ev) != proto.TypeOrderRequest || ev.PubKey != to {
					continue
				}
				var r proto.OrderRequest
				if json.Unmarshal([]byte(ev.Content), &r) == nil && r.ReplyP2P != nil && r.ReplyP2P.Validate() == nil {
					return r.ReplyP2P
				}
			}
		}
	}
	return nil
}

// DirectBackoff is how long after a failed P2P delivery to a peer messages to it go straight to the mailbox, so
// that an unreachable address does not cost every message the 10 seconds of §4.2.
var DirectBackoff = time.Minute

// directFailed remembers failed P2P deliveries (pubkey → when), bounded.
type directFailed struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (d *directFailed) recent(pk string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.at[pk]
	return ok && now.Sub(t) < DirectBackoff
}

func (d *directFailed) set(pk string, failed bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !failed {
		delete(d.at, pk)
		return
	}
	if d.at == nil {
		d.at = map[string]time.Time{}
	}
	if len(d.at) >= 4096 {
		for k, t := range d.at {
			if now.Sub(t) >= DirectBackoff {
				delete(d.at, k)
			}
		}
	}
	d.at[pk] = now
}

// sendDirect is the messenger's P2P path (§4.2): /ps/msg/1.0.0 to the recipient's known address.
func (n *Node) sendDirect(ctx context.Context, to, orderID string, wrap *nostr.Event) (bool, error) {
	c := n.contactOf(to, orderID)
	if c == nil || n.failed.recent(to, time.Now()) {
		return false, nil
	}
	ok, err := n.sendDirectTo(ctx, c, wrap)
	n.failed.set(to, err != nil, time.Now())
	return ok, err
}

func (n *Node) sendDirectTo(ctx context.Context, c *proto.P2PContact, wrap *nostr.Event) (bool, error) {
	id, addrs, err := p2p.ParseContact(p2p.Contact{PeerID: c.PeerID, Addrs: c.Addrs}, n.cfg.Nostr.AllowPrivateRelays)
	if err != nil {
		return false, err
	}
	if id == n.host.ID() {
		return false, nil
	}
	if err := n.p2p.SendMessage(ctx, id, addrs, wrap); err != nil {
		return false, err
	}
	return true, nil
}

// receiveDirect takes a wrap of /ps/msg/1.0.0 into the messenger.
func (n *Node) receiveDirect(wrap *nostr.Event, _ peer.ID) error {
	return n.msgr.ReceiveDirect(wrap)
}

// AddrRepublishInterval is the shortest time between two profiles published because our relay reservations
// changed (§3, §12: the profile carries the /p2p-circuit addresses).
var AddrRepublishInterval = 2 * time.Minute

// addrLoop publishes the profile again when the relay reservations changed, at most every AddrRepublishInterval.
func (n *Node) addrLoop(ctx context.Context) {
	if n.shopper == nil && n.escrow == nil {
		return
	}
	last := time.Now() // publishOwnLoop publishes at start
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.host.Changes():
		}
		wait := max(time.Until(last.Add(n.addrWait)), 2*time.Second) // a few reservations come together
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		// drain what came meanwhile: this publication covers it
		select {
		case <-n.host.Changes():
		default:
		}
		last = time.Now()
		if err := n.publishOwn(ctx); err != nil {
			n.log.Warn("publishing own events after an address change", "err", err)
		}
	}
}

// p2pRelaysLoop dials and reserves on the p2p_relays of the effective lists (§2.3), as they change.
func (n *Node) p2pRelaysLoop(ctx context.Context) {
	changes := n.trust.Watch()
	for {
		if relays := n.trust.P2PRelays(n.cfg.Trust.Coordinators, n.cfg.Network); len(relays) > 0 {
			n.host.AddRelays(relays)
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
		}
	}
}
