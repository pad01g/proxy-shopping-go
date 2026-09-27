package p2p

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// Stream protocols of §10.
const (
	ProtoStatus    = protocol.ID("/ps/status/1.0.0")
	ProtoTrustSync = protocol.ID("/ps/trust-sync/1.0.0")
)

const (
	maxEventSize   = 256 << 10
	maxSyncEvents  = 20000
	resyncInterval = 5 * time.Minute
	maxSynced      = 1024 // peers remembered for resyncInterval before the map is swept
)

// TopicTrust and TopicProfiles name the gossipsub topics of a network.
func TopicTrust(network string) string    { return "/ps/" + network + "/trust/1" }
func TopicProfiles(network string) string { return "/ps/" + network + "/profiles/1" }

// Status is the content of the kind 5401 status event.
type Status struct {
	PubKey       string           `json:"pubkey"`
	Role         string           `json:"role"`
	Network      string           `json:"network"`
	Reachability string           `json:"reachability"`
	Trust        map[string]int64 `json:"trust"`
	At           int64            `json:"at"`
}

// NewEventFunc is called for events that were new and newer than what the store had.
type NewEventFunc func(ev *nostr.Event, source string)

// Service runs gossip and the stream protocols on a host.
type Service struct {
	h       *Host
	ps      *pubsub.PubSub
	store   *trust.Store
	network string
	secret  string
	role    string
	log     *slog.Logger

	topics map[string]*pubsub.Topic

	mu     sync.Mutex
	onNew  []NewEventFunc
	synced map[peer.ID]time.Time
}

// ServiceOptions configure the service.
type ServiceOptions struct {
	Host    *Host
	Store   *trust.Store
	Network string
	Secret  string // identity key, signs status events
	Role    string
	Log     *slog.Logger
}

// NewService joins the topics and registers the stream handlers.
func NewService(ctx context.Context, o ServiceOptions) (*Service, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	ps, err := pubsub.NewGossipSub(ctx, o.Host,
		pubsub.WithPeerExchange(o.Role == "relay"),
		pubsub.WithMaxMessageSize(maxEventSize))
	if err != nil {
		return nil, fmt.Errorf("gossipsub: %w", err)
	}
	s := &Service{
		h: o.Host, ps: ps, store: o.Store, network: o.Network, secret: o.Secret, role: o.Role,
		log: o.Log.With("component", "gossip"), topics: map[string]*pubsub.Topic{}, synced: map[peer.ID]time.Time{},
	}
	for _, name := range []string{TopicTrust(o.Network), TopicProfiles(o.Network)} {
		if err := ps.RegisterTopicValidator(name, s.validator(name)); err != nil {
			return nil, fmt.Errorf("validator %s: %w", name, err)
		}
		t, err := ps.Join(name)
		if err != nil {
			return nil, fmt.Errorf("join %s: %w", name, err)
		}
		sub, err := t.Subscribe()
		if err != nil {
			return nil, fmt.Errorf("subscribe %s: %w", name, err)
		}
		s.topics[name] = t
		go s.readTopic(ctx, sub)
	}
	o.Host.SetStreamHandler(ProtoStatus, s.serveStatus)
	o.Host.SetStreamHandler(ProtoTrustSync, s.serveTrustSync)
	o.Host.Network().Notify(&network.NotifyBundle{ConnectedF: func(_ network.Network, c network.Conn) {
		go s.maybeSync(ctx, c.RemotePeer())
	}})
	return s, nil
}

// OnNewEvent registers a callback for newly stored events (the Nostr bridge).
func (s *Service) OnNewEvent(fn NewEventFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onNew = append(s.onNew, fn)
}

func (s *Service) notify(ev *nostr.Event, source string) {
	s.mu.Lock()
	fns := append([]NewEventFunc{}, s.onNew...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(ev, source)
	}
}

func topicFor(network string, kind int) (string, bool) {
	switch {
	case trust.IsTrustKind(kind):
		return TopicTrust(network), true
	case trust.IsProfileKind(kind):
		return TopicProfiles(network), true
	}
	return "", false
}

func (s *Service) validator(topic string) pubsub.Validator {
	return func(_ context.Context, _ peer.ID, msg *pubsub.Message) bool {
		var ev nostr.Event
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			return false
		}
		if t, ok := topicFor(s.network, ev.Kind); !ok || t != topic {
			return false
		}
		// only what our coordinators reach is stored and passed on (§10)
		return trust.Validate(&ev) == nil && s.store.InScope(&ev)
	}
}

func (s *Service) readTopic(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			return
		}
		if msg.ReceivedFrom == s.h.ID() {
			continue
		}
		var ev nostr.Event
		if json.Unmarshal(msg.Data, &ev) != nil {
			continue
		}
		s.accept(&ev, "p2p")
	}
}

// accept stores an event from libp2p and passes it on when it is newer.
func (s *Service) accept(ev *nostr.Event, source string) bool {
	newer, err := s.store.Put(ev)
	if err != nil {
		s.log.Debug("rejected event", "kind", ev.Kind, "source", source, "err", err)
		return false
	}
	if newer {
		s.log.Info("stored event", "kind", ev.Kind, "pubkey", ev.PubKey[:12], "v", trust.Version(ev), "source", source)
		s.notify(ev, source)
	}
	return newer
}

// Publish gossips an event on its topic.
func (s *Service) Publish(ctx context.Context, ev *nostr.Event) error {
	name, ok := topicFor(s.network, ev.Kind)
	if !ok {
		return fmt.Errorf("kind %d is not gossiped", ev.Kind)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := s.topics[name].Publish(ctx, data); err != nil {
		return fmt.Errorf("publish on %s: %w", name, err)
	}
	return nil
}

// Status returns our signed status event.
func (s *Service) Status() (*nostr.Event, error) {
	pub, err := nostr.GetPublicKey(s.secret)
	if err != nil {
		return nil, err
	}
	st := Status{
		PubKey: pub, Role: s.role, Network: s.network, Reachability: s.h.Reachability(),
		Trust: s.store.Versions(s.network), At: time.Now().Unix(),
	}
	content, _ := json.Marshal(st)
	ev := &nostr.Event{Kind: trust.KindStatus, CreatedAt: nostr.Now(), Tags: nostr.Tags{}, Content: string(content)}
	if err := ev.Sign(s.secret); err != nil {
		return nil, err
	}
	return ev, nil
}

func (s *Service) serveStatus(st network.Stream) {
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	ev, err := s.Status()
	if err != nil {
		_ = st.Reset()
		return
	}
	_ = json.NewEncoder(st).Encode(ev)
}

// QueryStatus asks a peer for its status event and verifies it.
func (s *Service) QueryStatus(ctx context.Context, id peer.ID) (*nostr.Event, *Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.h.ConnectPeer(ctx, id); err != nil {
		return nil, nil, err
	}
	st, err := s.h.NewStream(network.WithAllowLimitedConn(ctx, "ps-status"), id, ProtoStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("open status stream: %w", err)
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(15 * time.Second))
	var ev nostr.Event
	if err := json.NewDecoder(io.LimitReader(st, maxEventSize)).Decode(&ev); err != nil {
		return nil, nil, fmt.Errorf("read status: %w", err)
	}
	if ev.Kind != trust.KindStatus {
		return nil, nil, fmt.Errorf("status event has kind %d", ev.Kind)
	}
	if err := giftwrap.Verify(&ev); err != nil {
		return nil, nil, fmt.Errorf("status: %w", err)
	}
	var status Status
	if err := json.Unmarshal([]byte(ev.Content), &status); err != nil {
		return nil, nil, fmt.Errorf("status content: %w", err)
	}
	if status.PubKey != ev.PubKey {
		return nil, nil, errors.New("status pubkey differs from its signer")
	}
	return &ev, &status, nil
}

func (s *Service) serveTrustSync(st network.Stream) {
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(60 * time.Second))
	w := bufio.NewWriter(st)
	enc := json.NewEncoder(w)
	for _, ev := range s.store.All() {
		if trust.Network(ev) != s.network && ev.Kind != trust.KindInboxRelays {
			continue
		}
		if err := enc.Encode(ev); err != nil {
			_ = st.Reset()
			return
		}
	}
	_ = w.Flush()
}

// SyncFrom reads all trust and profile events of a peer and returns how many were new.
func (s *Service) SyncFrom(ctx context.Context, id peer.ID) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st, err := s.h.NewStream(network.WithAllowLimitedConn(ctx, "ps-sync"), id, ProtoTrustSync)
	if err != nil {
		return 0, fmt.Errorf("open trust-sync stream: %w", err)
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(60 * time.Second))
	dec := json.NewDecoder(bufio.NewReader(io.LimitReader(st, maxSyncEvents*maxEventSize/16)))
	n := 0
	for i := 0; i < maxSyncEvents; i++ {
		var ev nostr.Event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return n, fmt.Errorf("trust-sync from %s: %w", id, err)
		}
		if s.accept(&ev, "p2p-sync") {
			n++
		}
	}
	return n, nil
}

func (s *Service) maybeSync(ctx context.Context, id peer.ID) {
	s.mu.Lock()
	now := time.Now()
	if now.Sub(s.synced[id]) < resyncInterval {
		s.mu.Unlock()
		return
	}
	if len(s.synced) >= maxSynced {
		// entries older than the interval no longer suppress anything
		for p, at := range s.synced {
			if now.Sub(at) >= resyncInterval {
				delete(s.synced, p)
			}
		}
	}
	s.synced[id] = now
	s.mu.Unlock()
	// wait for identify, so that we know whether the peer speaks our protocol
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	if protos, err := s.h.Peerstore().SupportsProtocols(id, ProtoTrustSync); err != nil || len(protos) == 0 {
		return
	}
	n, err := s.SyncFrom(ctx, id)
	if err != nil {
		s.log.Debug("trust-sync failed", "peer", id, "err", err)
		return
	}
	if n > 0 {
		s.log.Info("trust-sync", "peer", id, "new", n)
	}
}

// Peers lists connected peers.
func (s *Service) Peers() []string {
	var out []string
	for _, p := range s.h.Network().Peers() {
		out = append(out, p.String())
	}
	return out
}
