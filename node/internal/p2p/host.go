// Package p2p is the libp2p side of a Go node (spec §10): a host with TCP and WebSocket, AutoNAT, circuit relay v2
// (client, and service for relay nodes), hole punching, the gossipsub topics for trust and profile events, and
// the status and trust-sync stream protocols.
package p2p

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
)

// Options configure the host.
type Options struct {
	Key          crypto.PrivKey
	Listen       []string
	Bootstrap    []string // multiaddrs with /p2p/<id>
	Relays       []string // static circuit relays
	Reachability string   // auto | public | private
	RelayService bool     // run a circuit relay v2 service
	Log          *slog.Logger
}

// Host is a running libp2p host.
type Host struct {
	host.Host
	log       *slog.Logger
	bootstrap []peer.AddrInfo
	relays    []peer.AddrInfo

	mu       sync.RWMutex
	reach    network.Reachability
	reserved map[peer.ID]time.Time // relay → expiry of our reservation
}

func parseAddrInfos(list []string) ([]peer.AddrInfo, error) {
	var out []peer.AddrInfo
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		info, err := peer.AddrInfoFromString(s)
		if err != nil {
			return nil, fmt.Errorf("peer address %q: %w", s, err)
		}
		out = append(out, *info)
	}
	return out, nil
}

// NewHost starts the libp2p host.
func NewHost(o Options) (*Host, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	boot, err := parseAddrInfos(o.Bootstrap)
	if err != nil {
		return nil, err
	}
	relays, err := parseAddrInfos(o.Relays)
	if err != nil {
		return nil, err
	}
	listen := libp2p.ListenAddrStrings(o.Listen...)
	if len(o.Listen) == 0 {
		listen = libp2p.NoListenAddrs // reachable through relays only
	}
	opts := []libp2p.Option{
		libp2p.Identity(o.Key),
		listen,
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(websocket.New),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
		libp2p.EnableNATService(),
	}
	if o.RelayService {
		res := relay.DefaultResources()
		// the defaults (2 minutes, 128 KiB per circuit) are too small for trust-sync of a large store
		res.Limit = &relay.RelayLimit{Duration: 30 * time.Minute, Data: 64 << 20}
		res.MaxReservations = 1024
		res.MaxCircuits = 64
		res.MaxReservationsPerIP = 64
		res.MaxReservationsPerASN = 256
		opts = append(opts, libp2p.EnableRelayService(relay.WithResources(res)))
	} else if len(relays) > 0 {
		opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(relays,
			autorelay.WithBootDelay(0), autorelay.WithMinCandidates(1), autorelay.WithNumRelays(len(relays)),
			autorelay.WithBackoff(15*time.Second)))
	}
	reach := network.ReachabilityUnknown
	switch o.Reachability {
	case "public":
		opts = append(opts, libp2p.ForceReachabilityPublic())
		reach = network.ReachabilityPublic
	case "private":
		opts = append(opts, libp2p.ForceReachabilityPrivate())
		reach = network.ReachabilityPrivate
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}
	ph := &Host{Host: h, log: o.Log.With("component", "p2p"), bootstrap: boot, relays: relays, reach: reach, reserved: map[peer.ID]time.Time{}}
	ph.log.Info("libp2p host started", "peer_id", h.ID().String(), "addrs", h.Addrs(), "relay_service", o.RelayService)
	return ph, nil
}

// Start keeps connections to the bootstrap peers and relays and tracks reachability.
func (h *Host) Start(ctx context.Context) {
	go h.watchReachability(ctx)
	seen := map[peer.ID]bool{}
	for _, list := range [][]peer.AddrInfo{h.bootstrap, h.relays} {
		for _, p := range list {
			if seen[p.ID] || p.ID == h.ID() {
				continue
			}
			seen[p.ID] = true
			go h.keepConnected(ctx, p)
		}
	}
	for _, r := range h.relays {
		if r.ID != h.ID() {
			go h.reserveLoop(ctx, r)
		}
	}
}

// reserveLoop keeps a reservation on a relay while we are not known to be publicly reachable. AutoRelay only
// reserves after AutoNAT decided "private", which can take minutes or never happen with private addresses.
func (h *Host) reserveLoop(ctx context.Context, r peer.AddrInfo) {
	for ctx.Err() == nil {
		wait := 15 * time.Second
		if h.Reachability() != "public" {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			res, err := client.Reserve(rctx, h.Host, r)
			cancel()
			if err != nil {
				h.log.Debug("relay reservation failed", "relay", r.ID, "err", err)
			} else {
				h.mu.Lock()
				first := h.reserved[r.ID].IsZero()
				h.reserved[r.ID] = res.Expiration
				h.mu.Unlock()
				if first {
					h.log.Info("reserved a slot on relay", "relay", r.ID, "until", res.Expiration.Format(time.RFC3339))
				}
				wait = min(max(time.Until(res.Expiration)-2*time.Minute, 30*time.Second), 5*time.Minute)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// CircuitAddrs lists the circuit addresses of our current relay reservations.
func (h *Host) CircuitAddrs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []string
	for _, r := range h.relays {
		if time.Now().After(h.reserved[r.ID]) || h.Network().Connectedness(r.ID) != network.Connected {
			continue
		}
		for _, a := range r.Addrs {
			out = append(out, a.String()+"/p2p/"+r.ID.String()+"/p2p-circuit/p2p/"+h.ID().String())
		}
	}
	return out
}

func (h *Host) keepConnected(ctx context.Context, p peer.AddrInfo) {
	for ctx.Err() == nil {
		if h.Network().Connectedness(p.ID) != network.Connected {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := h.Connect(cctx, p); err != nil {
				h.log.Debug("bootstrap peer unreachable", "peer", p.ID, "err", err)
			} else {
				h.log.Info("connected to bootstrap peer", "peer", p.ID)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
}

func (h *Host) watchReachability(ctx context.Context) {
	sub, err := h.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged))
	if err != nil {
		h.log.Warn("reachability events unavailable", "err", err)
		return
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub.Out():
			r := e.(event.EvtLocalReachabilityChanged).Reachability
			h.mu.Lock()
			h.reach = r
			h.mu.Unlock()
			h.log.Info("reachability changed", "reachability", r.String())
		}
	}
}

// Reachability is "public", "private" or "unknown".
func (h *Host) Reachability() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return strings.ToLower(h.reach.String())
}

// FullAddrs lists our addresses with the /p2p/<id> suffix, circuit addresses included.
func (h *Host) FullAddrs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, a := range h.Addrs() {
		add(a.String() + "/p2p/" + h.ID().String())
	}
	for _, a := range h.CircuitAddrs() {
		add(a)
	}
	return out
}

// Connect dials a peer, trying circuits through our relays when no direct address is known or works.
func (h *Host) ConnectPeer(ctx context.Context, id peer.ID) error {
	if h.Network().Connectedness(id) == network.Connected {
		return nil
	}
	for _, r := range h.relays {
		circuit, err := ma.NewMultiaddr("/p2p/" + r.ID.String() + "/p2p-circuit")
		if err != nil {
			continue
		}
		for _, ra := range r.Addrs {
			h.Peerstore().AddAddr(id, ra.Encapsulate(circuit), peerstore.TempAddrTTL)
		}
	}
	ctx = network.WithAllowLimitedConn(ctx, "ps")
	if err := h.Connect(ctx, peer.AddrInfo{ID: id}); err != nil {
		return fmt.Errorf("connect %s: %w", id, err)
	}
	return nil
}
