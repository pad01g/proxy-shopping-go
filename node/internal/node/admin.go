package node

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/escrow"
	"github.com/pad01g/proxy-shopping-go/node/internal/shopper"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// AdminTokenFile is the file in data_dir holding the generated admin token.
const AdminTokenFile = "admin.token"

// adminToken returns the configured token, or the one generated at the first start into data_dir/admin.token.
func adminToken(cfg *config.Config, log *slog.Logger) (string, error) {
	if cfg.Admin.Token != "" || cfg.Admin.Listen == "" {
		return cfg.Admin.Token, nil
	}
	path := filepath.Join(cfg.DataDir, AdminTokenFile)
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return strings.TrimSpace(string(data)), nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return "", fmt.Errorf("admin token: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("admin token: %w", err)
	}
	log.Warn("admin.token is not configured: generated one; send it as Authorization: Bearer <token>", "file", path)
	return token, nil
}

// guard protects the admin API against other web pages in the operator's browser (CSRF, DNS rebinding): the Host
// must be localhost, an IP address or one of admin.hosts; a cross-origin Origin is refused; POST bodies must be
// JSON; and every request but GET /healthz needs the bearer token.
func (n *Node) guard(next http.Handler) http.Handler {
	token := n.adminToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		if !n.allowedHost(r.Host) {
			writeError(w, http.StatusForbidden, errors.New("host not allowed (admin.hosts)"))
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			writeError(w, http.StatusForbidden, errors.New("cross-origin requests are not allowed"))
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
				return
			}
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("bearer token required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Node) allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || net.ParseIP(host) != nil {
		return true // an IP address cannot be rebound to another server
	}
	for _, h := range n.cfg.Admin.Hosts {
		if strings.EqualFold(strings.TrimSuffix(h, "."), host) {
			return true
		}
	}
	return false
}

// sameOrigin tells whether an Origin header names the host the request was sent to.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, host)
}

func (n *Node) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if !n.started.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": n.cfg.Role})
}

// handlePause is POST /admin/pause {"seconds": n}: the node stops its message traffic and tick loops for n seconds
// (Node.Pause). POST /admin/resume ends the pause.
func (n *Node) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int64 `json:"seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	until, err := n.Pause(time.Duration(req.Seconds) * time.Second)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"paused": true, "paused_until": until.Unix()})
}

func (n *Node) handleResume(w http.ResponseWriter, _ *http.Request) {
	was := n.Resume()
	writeJSON(w, http.StatusOK, map[string]any{"paused": false, "was_paused": was})
}

func (n *Node) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", n.handleHealthz)
	mux.HandleFunc("POST /admin/pause", n.handlePause)
	mux.HandleFunc("POST /admin/resume", n.handleResume)
	mux.HandleFunc("GET /status", n.handleStatus)
	mux.HandleFunc("GET /trust", n.handleTrust)
	mux.HandleFunc("POST /events", n.handleEvents)
	mux.HandleFunc("POST /p2p/status", n.handleP2PStatus)
	switch {
	case n.shopper != nil:
		mux.HandleFunc("GET /orders", n.handleOrders)
		mux.HandleFunc("GET /orders/{id}", n.handleOrder)
		mux.HandleFunc("POST /orders/{id}/resolve", n.handleResolve)
	case n.escrow != nil:
		mux.HandleFunc("GET /cases", n.handleCases)
		mux.HandleFunc("GET /cases/{id}", n.handleCase)
		mux.HandleFunc("POST /cases/{id}/rule", n.handleRule)
	case n.operator != nil:
		mux.HandleFunc("GET /reports", n.handleReports)
	}
	return n.guard(mux)
}

// Status is GET /status.
type Status struct {
	PubKey       string           `json:"pubkey"`
	Name         string           `json:"name"`
	Role         string           `json:"role"`
	Network      string           `json:"network"`
	PeerID       string           `json:"peer_id"`
	Addrs        []string         `json:"addrs"`
	Reachability string           `json:"reachability"`
	Trust        map[string]int64 `json:"trust"`
	Relays       []string         `json:"relays"`
	P2PRelays    []string         `json:"p2p_relays"`
	Peers        []string         `json:"peers"`
	Pending      int              `json:"pending_messages"`
	PausedUntil  int64            `json:"paused_until,omitempty"` // POST /admin/pause
}

func (n *Node) status() Status {
	st := Status{
		PubKey: n.keys.NostrPubHex(), Name: n.cfg.Name, Role: n.cfg.Role, Network: n.cfg.Network,
		PeerID: n.host.ID().String(), Addrs: n.host.FullAddrs(), Reachability: n.host.Reachability(),
		Trust: n.trust.Versions(n.cfg.Network), Relays: n.cfg.Nostr.Relays, P2PRelays: n.cfg.P2P.Relays,
		Peers: n.p2p.Peers(),
	}
	if n.msgr != nil {
		st.Pending = len(n.msgr.Pending())
	}
	if t := n.PausedUntil(); !t.IsZero() {
		st.PausedUntil = t.Unix()
	}
	return st
}

func (n *Node) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.status())
}

func (n *Node) handleTrust(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"network":      n.cfg.Network,
		"coordinators": n.cfg.Trust.Coordinators,
		"events":       n.trust.All(),
		"effective":    n.trust.Effective(n.cfg.Trust.Coordinators, n.cfg.Network),
	})
}

// handleEvents accepts one event or an array of events.
func (n *Node) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var evs []*nostr.Event
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(body, &evs)
	} else {
		var ev nostr.Event
		err = json.Unmarshal(body, &ev)
		evs = []*nostr.Event{&ev}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	type result struct {
		ID    string `json:"id"`
		Newer bool   `json:"newer"`
		Error string `json:"error,omitempty"`
	}
	var out []result
	status := http.StatusOK
	for _, ev := range evs {
		if ev == nil {
			out = append(out, result{Error: "null event"})
			status = http.StatusBadRequest
			continue
		}
		newer, err := n.PublishEvent(r.Context(), ev)
		res := result{ID: ev.ID, Newer: newer}
		if err != nil {
			res.Error, status = err.Error(), http.StatusBadRequest
		}
		out = append(out, res)
	}
	writeJSON(w, status, out)
}

func (n *Node) handleP2PStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, err := peer.Decode(req.PeerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ev, st, err := n.p2p.QueryStatus(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	var viaRelay bool
	for _, c := range n.host.Network().ConnsToPeer(id) {
		viaRelay = viaRelay || c.Stat().Limited
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "status": st, "limited_connection": viaRelay})
}

// orderSummary is one line of GET /orders.
type orderSummary struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	User       string `json:"user"`
	Asset      string `json:"asset,omitempty"`
	Lock       string `json:"lock_amount,omitempty"`
	ShipStatus string `json:"ship_status,omitempty"`
	PayoutTx   string `json:"payout_tx,omitempty"`
	Updated    int64  `json:"updated"`
	Error      string `json:"error,omitempty"`
}

func (n *Node) handleOrders(w http.ResponseWriter, _ *http.Request) {
	orders, err := n.shopper.Orders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].Updated > orders[j].Updated })
	out := []orderSummary{}
	for _, o := range orders {
		s := orderSummary{ID: o.ID, State: o.State, User: o.User, ShipStatus: o.ShipStatus, PayoutTx: o.PayoutTx, Updated: o.Updated, Error: o.Error}
		if o.Quote != nil {
			s.Asset, s.Lock = o.Quote.Asset, o.Quote.LockAmount
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) handleOrder(w http.ResponseWriter, r *http.Request) {
	o, ok, err := n.shopper.Order(r.PathValue("id"))
	if err != nil || !ok {
		writeError(w, http.StatusNotFound, errors.New("no such order"))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*shopper.Order
		Messages []*nostr.Event `json:"messages"`
	}{o, append(n.msgr.Inbox(o.ID), n.msgr.Outbox(o.ID)...)})
}

// handleResolve settles an order the bot left to a human (shopper.ResolveRequest).
func (n *Node) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req shopper.ResolveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	o, err := n.shopper.Resolve(r.Context(), r.PathValue("id"), req)
	switch {
	case errors.Is(err, shopper.ErrNotResolvable):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, o)
	}
}

func (n *Node) handleCases(w http.ResponseWriter, _ *http.Request) {
	cases, err := n.escrow.Cases()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cases == nil {
		cases = []escrow.Case{}
	}
	writeJSON(w, http.StatusOK, cases)
}

func (n *Node) handleCase(w http.ResponseWriter, r *http.Request) {
	c, ok, err := n.escrow.Case(r.PathValue("id"))
	if err != nil || !ok {
		writeError(w, http.StatusNotFound, errors.New("no such case"))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (n *Node) handleRule(w http.ResponseWriter, r *http.Request) {
	var req escrow.RuleRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ruling, err := n.escrow.Rule(r.Context(), r.PathValue("id"), req)
	switch {
	case errors.Is(err, escrow.ErrNoObligation), errors.Is(err, escrow.ErrAlreadyRuled):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, ruling)
	}
}

func (n *Node) handleReports(w http.ResponseWriter, _ *http.Request) {
	reports, err := n.operator.Reports()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, reports)
}
