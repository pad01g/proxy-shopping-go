package node

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// Inbox relays (10050) of pubkeys outside the trust scope, e.g. the users we have orders with, are looked up
// when a message is sent to them and cached here, bounded, instead of being stored and gossiped (§10).
var (
	InboxCacheSize = 1000
	InboxCacheTTL  = 10 * time.Minute
	inboxLookup    = 5 * time.Second
)

type inboxCache struct {
	trust  *trust.Store
	pool   *nostrnet.Pool
	relays func() []string

	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recent
}

type cachedInbox struct {
	pubkey string
	relays []string
	at     time.Time
}

func newInboxCache(ts *trust.Store, pool *nostrnet.Pool, relays func() []string) *inboxCache {
	return &inboxCache{trust: ts, pool: pool, relays: relays, entries: map[string]*list.Element{}, lru: list.New()}
}

// resolve returns the inbox relays of a pubkey: from the trust store, else the cache, else a query to our relays.
func (c *inboxCache) resolve(pubkey string) []string {
	if r := c.trust.InboxRelaysOf(pubkey); len(r) > 0 {
		return r
	}
	c.mu.Lock()
	if el, ok := c.entries[pubkey]; ok {
		e := el.Value.(*cachedInbox)
		if time.Since(e.at) < InboxCacheTTL {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return e.relays
		}
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), inboxLookup)
	defer cancel()
	var newest *nostr.Event
	for _, ev := range c.pool.Query(ctx, c.relays(), nostr.Filter{Kinds: []int{trust.KindInboxRelays}, Authors: []string{pubkey}, Limit: 5}) {
		if ev.PubKey != pubkey || trust.Validate(ev) != nil {
			continue
		}
		if newest == nil || trust.Newer(ev, newest) {
			newest = ev
		}
	}
	var relays []string
	if newest != nil {
		relays = trust.InboxRelays(newest)
	}
	c.put(pubkey, relays) // also a miss, so that an unknown pubkey is not asked for on every send
	return relays
}

func (c *inboxCache) put(pubkey string, relays []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[pubkey]; ok {
		el.Value = &cachedInbox{pubkey, relays, time.Now()}
		c.lru.MoveToFront(el)
		return
	}
	c.entries[pubkey] = c.lru.PushFront(&cachedInbox{pubkey, relays, time.Now()})
	for c.lru.Len() > max(InboxCacheSize, 1) {
		last := c.lru.Back()
		c.lru.Remove(last)
		delete(c.entries, last.Value.(*cachedInbox).pubkey)
	}
}

func (c *inboxCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}
