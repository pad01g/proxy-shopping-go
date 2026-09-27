// Package node wires a psnode together: keys, store, trust, Nostr, libp2p, the role engine and the admin API.
package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/botclient"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/escrow"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/fx"
	"github.com/pad01g/proxy-shopping-go/node/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/operator"
	"github.com/pad01g/proxy-shopping-go/node/internal/p2p"
	"github.com/pad01g/proxy-shopping-go/node/internal/shop"
	"github.com/pad01g/proxy-shopping-go/node/internal/shopper"
	"github.com/pad01g/proxy-shopping-go/node/internal/store"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// ProfileInterval is how often a node republishes its profile and inbox relays.
var ProfileInterval = 10 * time.Minute

// Node is a running psnode.
type Node struct {
	cfg  *config.Config
	keys *keys.Set
	log  *slog.Logger
	db   *store.DB
	tls  *tls.Config
	http *http.Client

	trust *trust.Store
	pool  *nostrnet.Pool
	host  *p2p.Host
	p2p   *p2p.Service
	msgr  *messenger.Messenger

	inboxes  *inboxCache
	debounce time.Duration // of the bridge (BridgeDebounce at New)

	esplora *btc.Esplora
	evm     *evm.Client

	shopper  *shopper.Engine
	escrow   *escrow.Engine
	operator *operator.Inbox

	depMu sync.Mutex
	deps  *evm.Deployments
}

// New builds a node from its configuration; Run starts it.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Node, error) {
	if log == nil {
		log = slog.Default()
	}
	n := &Node{cfg: cfg, log: log.With("node", cfg.Name, "role", cfg.Role), debounce: BridgeDebounce}
	var err error
	if n.keys, err = keys.LoadMnemonicFile(cfg.MnemonicFile); err != nil {
		return nil, err
	}
	if cfg.TLS.ExtraCA != "" {
		if err := httpx.WaitForFile(cfg.TLS.ExtraCA, 2*time.Minute); err != nil {
			return nil, fmt.Errorf("tls.extra_ca: %w", err)
		}
	}
	if n.tls, err = httpx.TLSConfig(cfg.TLS.ExtraCA); err != nil {
		return nil, err
	}
	n.http = httpx.Client(n.tls, 30*time.Second)
	if n.db, err = store.Open(filepath.Join(cfg.DataDir, "node.db")); err != nil {
		return nil, err
	}
	if n.trust, err = trust.NewStore(n.db); err != nil {
		return nil, err
	}
	// §10: keep only what the coordinators reach, and our own profile and inbox relays
	if dropped := n.trust.SetScope(cfg.Trust.Coordinators, cfg.Network, n.keys.NostrPubHex()); dropped > 0 {
		n.log.Info("dropped stored events outside the trust scope", "count", dropped)
	}
	n.pool = nostrnet.NewPoolWith(nostrnet.Options{TLS: n.tls, Log: n.log, AllowPrivate: cfg.Nostr.AllowPrivateRelays})
	n.inboxes = newInboxCache(n.trust, n.pool, func() []string { return n.cfg.Nostr.Relays })

	key, err := n.keys.Libp2pKey()
	if err != nil {
		return nil, err
	}
	n.host, err = p2p.NewHost(p2p.Options{
		Key: key, Listen: cfg.P2P.Listen, Bootstrap: cfg.P2P.Bootstrap, Relays: cfg.P2P.Relays,
		Reachability: cfg.P2P.Reachability, RelayService: cfg.Role == config.RoleRelay, Log: n.log,
	})
	if err != nil {
		return nil, err
	}
	n.p2p, err = p2p.NewService(ctx, p2p.ServiceOptions{
		Host: n.host, Store: n.trust, Network: cfg.Network, Secret: n.keys.NostrSecretHex(), Role: cfg.Role, Log: n.log,
	})
	if err != nil {
		return nil, err
	}

	if c := cfg.Chain.BTC; c != nil && c.Esplora != "" {
		n.esplora = btc.NewEsplora(c.Esplora, n.http)
	}
	if c := cfg.Chain.EVM; c != nil && c.RPC != "" {
		if n.evm, err = evm.Dial(ctx, c.RPC, n.http); err != nil {
			return nil, err
		}
	}
	if cfg.Role != config.RoleRelay {
		n.msgr, err = messenger.New(messenger.Config{
			Secret: n.keys.NostrSecretHex(), Pool: n.pool, DB: n.db, Inbox: cfg.Nostr.Relays, K: cfg.Nostr.K,
			Resolve: n.inboxes.resolve, Retry: n.hasOrderWith, Log: n.log,
		})
		if err != nil {
			return nil, err
		}
	}
	if err := n.buildRole(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) buildRole() error {
	cfg := n.cfg
	switch cfg.Role {
	case config.RoleShopper:
		providers, err := fx.FromConfig(cfg.FX.Sources, cfg.Shopper.Currencies, n.http, n.evmCaller(), n.feeds())
		if err != nil {
			return err
		}
		d := shopper.Deps{
			Keys: n.keys, Messenger: n.msgr, Trust: n.trust, Coordinators: cfg.Trust.Coordinators, Network: cfg.Network,
			FX: fx.New(providers, n.log), EVM: n.evm, Bot: botclient.New(cfg.Shopper.BotURL),
			Shops: shop.NewInspector(n.tls), DB: n.db, Config: cfg.Shopper, Name: cfg.Name, Log: n.log,
		}
		if n.esplora != nil {
			d.BTC = n.esplora
		}
		if n.evm != nil {
			d.Deployments = n.deployments
		}
		n.shopper = shopper.New(d)
	case config.RoleEscrow:
		d := escrow.Deps{
			Keys: n.keys, Messenger: n.msgr, Trust: n.trust, Network: cfg.Network, EVM: n.evm,
			DB: n.db, Config: cfg.Escrow, Name: cfg.Name, Log: n.log,
		}
		if n.esplora != nil {
			d.BTC = n.esplora
		}
		if n.evm != nil {
			d.Deployments = n.deployments
		}
		n.escrow = escrow.New(d)
	case config.RoleOperator:
		n.operator = operator.New(n.msgr, n.db, n.log)
	}
	return nil
}

// evmCaller is the chainlink provider's view of the EVM client (nil without EVM).
func (n *Node) evmCaller() fx.Caller {
	if n.evm == nil {
		return nil
	}
	return n.evm
}

// feeds returns the oracle feeds of the deployments. A chainlink source without its own feeds needs them, so
// then we wait a while for the deployer to write the file.
func (n *Node) feeds() map[string]common.Address {
	needed := false
	for _, s := range n.cfg.FX.Sources {
		needed = needed || (s.Type == "chainlink" && len(s.Feeds) == 0)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		d, err := n.deployments()
		if err == nil {
			return d.Feeds
		}
		if !needed || time.Now().After(deadline) {
			return nil
		}
		n.log.Info("waiting for the deployments file (chainlink feeds)", "err", err)
		time.Sleep(2 * time.Second)
	}
}

// deployments loads the deployments file once it exists (the deployer may still be running at start).
func (n *Node) deployments() (*evm.Deployments, error) {
	n.depMu.Lock()
	defer n.depMu.Unlock()
	if n.deps != nil {
		return n.deps, nil
	}
	c := n.cfg.Chain.EVM
	if c == nil || c.Deployments == "" {
		return nil, errors.New("chain.evm.deployments is not configured")
	}
	d, err := evm.LoadDeployments(c.Deployments)
	if err != nil {
		return nil, err
	}
	if c.ChainID != 0 && d.ChainID != c.ChainID {
		return nil, fmt.Errorf("deployments are for chain %d, configured %d", d.ChainID, c.ChainID)
	}
	n.deps = d
	return d, nil
}

// Run starts everything and serves the admin API until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	defer n.db.Close()
	defer n.host.Close()
	defer n.pool.Close()

	n.host.Start(ctx)
	n.bridge(ctx)
	if n.msgr != nil {
		n.msgr.Start(ctx)
	}
	if n.shopper != nil {
		n.shopper.Start(ctx)
	}
	if n.escrow != nil {
		n.escrow.Start(ctx)
	}
	go n.publishOwnLoop(ctx)

	id := n.host.ID().String()
	n.log.Info("psnode running", "pubkey", n.keys.NostrPubHex(), "peer_id", id, "admin", n.cfg.Admin.Listen)
	if n.cfg.Admin.Listen == "" {
		<-ctx.Done()
		return nil
	}
	srv := &http.Server{Addr: n.cfg.Admin.Listen, Handler: n.adminHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		return fmt.Errorf("admin api: %w", err)
	}
}

// bridge passes newer trust and profile events between libp2p and the Nostr relays. It subscribes only to the
// authors of the trust scope (§10) and subscribes again when the scope changes.
func (n *Node) bridge(ctx context.Context) {
	n.p2p.OnNewEvent(func(ev *nostr.Event, source string) {
		if source == "nostr" || source == "local" {
			return
		}
		go n.publishNostr(ctx, ev)
	})
	go n.bridgeLoop(ctx)
}

// BridgeDebounce is how long the bridge waits for more trust changes before it subscribes again.
var BridgeDebounce = time.Second

func (n *Node) bridgeLoop(ctx context.Context) {
	var current nostr.Filters
	stop := func() {}
	defer func() { stop() }()
	for {
		if fs := n.bridgeFilters(); !reflect.DeepEqual(fs, current) {
			stop()
			current = fs
			sctx, cancel := context.WithCancel(ctx)
			stop = cancel
			if len(fs) > 0 {
				n.log.Debug("bridge subscription", "filters", len(fs))
				n.pool.Subscribe(sctx, n.cfg.Nostr.Relays, fs, func(_ string, ev *nostr.Event) { n.bridgeEvent(ctx, ev) })
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-n.trust.Changes():
		}
		select { // a list and its profiles tend to arrive together
		case <-ctx.Done():
			return
		case <-time.After(n.debounce):
		}
	}
}

// bridgeFilters asks for the delegations of the coordinators, the lists of the delegated operators and the
// profiles and inbox relays of the shoppers and escrows of the effective set (and our own).
func (n *Node) bridgeFilters() nostr.Filters {
	a, ok := n.trust.Authors()
	if !ok {
		return nil
	}
	var fs nostr.Filters
	// an empty authors list would mean everybody
	if len(a.Coordinators) > 0 {
		fs = append(fs, nostr.Filter{Kinds: []int{trust.KindDelegation}, Authors: a.Coordinators})
	}
	if len(a.Operators) > 0 {
		fs = append(fs, nostr.Filter{Kinds: []int{trust.KindList}, Authors: a.Operators})
	}
	if len(a.Participants) > 0 {
		fs = append(fs, nostr.Filter{Kinds: []int{trust.KindShopperProfile, trust.KindEscrowProfile, trust.KindInboxRelays}, Authors: a.Participants})
	}
	return fs
}

func (n *Node) bridgeEvent(ctx context.Context, ev *nostr.Event) {
	if trust.Network(ev) != "" && trust.Network(ev) != n.cfg.Network && ev.Kind != trust.KindInboxRelays {
		return
	}
	newer, err := n.trust.Put(ev)
	if err != nil || !newer {
		return
	}
	n.log.Info("stored event", "kind", ev.Kind, "pubkey", ev.PubKey[:12], "v", trust.Version(ev), "source", "nostr")
	if err := n.p2p.Publish(ctx, ev); err != nil {
		n.log.Debug("gossip failed", "err", err)
	}
}

// hasOrderWith tells the messenger whether unacknowledged messages about an order are resent: only to parties
// of an order we keep (§4.10; a rejected request is answered once).
func (n *Node) hasOrderWith(_, orderID string) bool {
	if orderID == "" {
		return false
	}
	switch {
	case n.shopper != nil:
		// a rejection is still a reply to a real requester, so it is resent until acked too
		_, ok, err := n.shopper.Order(orderID)
		return err == nil && ok
	case n.escrow != nil:
		_, ok, err := n.escrow.Case(orderID)
		return err == nil && ok
	}
	return false
}

func (n *Node) publishNostr(ctx context.Context, ev *nostr.Event) {
	if len(n.cfg.Nostr.Relays) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := n.pool.Publish(pctx, n.cfg.Nostr.Relays, ev); err != nil {
		n.log.Debug("publish to nostr", "kind", ev.Kind, "err", err)
	}
}

// PublishEvent validates, stores, gossips and publishes a signed trust or profile event.
func (n *Node) PublishEvent(ctx context.Context, ev *nostr.Event) (bool, error) {
	newer, err := n.trust.Put(ev)
	if err != nil {
		return false, err
	}
	// gossip and publish even when we knew the event, so that a re-post repairs relays that lost it
	if err := n.p2p.Publish(ctx, ev); err != nil {
		n.log.Debug("gossip failed", "err", err)
	}
	n.publishNostr(ctx, ev)
	return newer, nil
}

type ownEvent struct {
	Event *nostr.Event `json:"event"`
}

// publishOwnLoop publishes our inbox relays and profile on start and every ProfileInterval.
func (n *Node) publishOwnLoop(ctx context.Context) {
	// give the circuit reservation a moment, the profile carries the addresses
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	t := time.NewTicker(ProfileInterval)
	defer t.Stop()
	for {
		if err := n.publishOwn(ctx); err != nil {
			n.log.Warn("publishing own events", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (n *Node) p2pInfo() *trust.P2PInfo {
	addrs := n.host.CircuitAddrs()
	for _, a := range n.host.FullAddrs() {
		if !contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	return &trust.P2PInfo{PeerID: n.host.ID().String(), Addrs: addrs}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (n *Node) publishOwn(ctx context.Context) error {
	sk := n.keys.NostrSecretHex()
	if n.msgr != nil && len(n.cfg.Nostr.Relays) > 0 {
		if err := n.publishVersioned(ctx, "10050", n.cfg.Nostr.Relays, func(v int64) (*nostr.Event, error) {
			return trust.NewInboxRelays(sk, n.cfg.Nostr.Relays, v)
		}); err != nil {
			return err
		}
	}
	switch {
	case n.shopper != nil:
		p := n.shopper.Profile(n.p2pInfo())
		return n.publishVersioned(ctx, "30502", p, func(v int64) (*nostr.Event, error) {
			return trust.NewProfile(sk, trust.KindShopperProfile, n.cfg.Network, v, p)
		})
	case n.escrow != nil:
		p, err := n.escrow.Profile(n.p2pInfo())
		if err != nil {
			return err
		}
		return n.publishVersioned(ctx, "30503", p, func(v int64) (*nostr.Event, error) {
			return trust.NewProfile(sk, trust.KindEscrowProfile, n.cfg.Network, v, p)
		})
	}
	return nil
}

// publishVersioned republishes the last event of a kind while its content is unchanged, and signs a new
// version (v = now) when it changed.
func (n *Node) publishVersioned(ctx context.Context, name string, content any, build func(v int64) (*nostr.Event, error)) error {
	type stored struct {
		Content any          `json:"content"`
		Event   *nostr.Event `json:"event"`
	}
	var last stored
	_, _ = n.db.Get("own", name, &last)
	current, _ := normalize(content)
	if last.Event == nil || !reflect.DeepEqual(last.Content, current) || n.trust.Get(trust.KeyOf(last.Event)) == nil {
		v := time.Now().Unix()
		if last.Event != nil && trust.Version(last.Event) >= v {
			v = trust.Version(last.Event) + 1
		}
		ev, err := build(v)
		if err != nil {
			return err
		}
		last = stored{Content: current, Event: ev}
		if err := n.db.Put("own", name, last); err != nil {
			return err
		}
		n.log.Info("signed new version", "kind", name, "v", strconv.FormatInt(v, 10))
	}
	_, err := n.PublishEvent(ctx, last.Event)
	return err
}

// normalize turns a value into its generic JSON form, so that it compares equal to a stored copy.
func normalize(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(data, &out)
}
