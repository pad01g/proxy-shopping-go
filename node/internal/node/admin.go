package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

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

func (n *Node) auth(next http.Handler) http.Handler {
	token := n.cfg.Admin.Token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("bearer token required"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Node) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", n.handleStatus)
	mux.HandleFunc("GET /trust", n.handleTrust)
	mux.HandleFunc("POST /events", n.handleEvents)
	mux.HandleFunc("POST /p2p/status", n.handleP2PStatus)
	switch {
	case n.shopper != nil:
		mux.HandleFunc("GET /orders", n.handleOrders)
		mux.HandleFunc("GET /orders/{id}", n.handleOrder)
	case n.escrow != nil:
		mux.HandleFunc("GET /cases", n.handleCases)
		mux.HandleFunc("GET /cases/{id}", n.handleCase)
		mux.HandleFunc("POST /cases/{id}/rule", n.handleRule)
	case n.operator != nil:
		mux.HandleFunc("GET /reports", n.handleReports)
	}
	return n.auth(mux)
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
	case errors.Is(err, escrow.ErrNoObligation):
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
