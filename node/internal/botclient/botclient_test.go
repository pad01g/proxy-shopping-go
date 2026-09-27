package botclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

func TestClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/purchase", func(w http.ResponseWriter, r *http.Request) {
		var req proto.PurchaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RequestID == "busy" {
			http.Error(w, `{"error":"in_progress"}`, http.StatusConflict)
			return
		}
		if req.RequestID == "down" {
			http.Error(w, "restarting", http.StatusBadGateway)
			return
		}
		if req.PaymentRef != "cash" {
			http.Error(w, "unsupported", http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(proto.PurchaseResult{RequestID: req.RequestID, Status: "ok", ShopOrderID: "X-1"})
	})
	mux.HandleFunc("POST /v1/tracking", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"shipped","tracking_no":"T","updated_at":1790000000,"evidence":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL + "/")
	ctx := context.Background()

	res, err := c.Purchase(ctx, proto.PurchaseRequest{RequestID: "r", PaymentRef: "cash"})
	if err != nil || res.ShopOrderID != "X-1" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := c.Purchase(ctx, proto.PurchaseRequest{PaymentRef: "card:default"}); err == nil || !strings.Contains(err.Error(), "422") || !Rejected(err) || InProgress(err) {
		t.Fatalf("error not surfaced: %v", err)
	}
	// 409: the bot is still buying this request_id; 5xx: try again later
	if _, err := c.Purchase(ctx, proto.PurchaseRequest{RequestID: "busy"}); !InProgress(err) || Rejected(err) {
		t.Fatalf("409: %v", err)
	}
	if _, err := c.Purchase(ctx, proto.PurchaseRequest{RequestID: "down"}); err == nil || InProgress(err) || Rejected(err) {
		t.Fatalf("502: %v", err)
	}
	st, err := c.Tracking(ctx, proto.TrackingQuery{ShopURL: "https://s", ShopOrderID: "X-1"})
	if err != nil || st.Status != "shipped" || string(st.UpdatedAt) != "1790000000" {
		t.Fatalf("%+v %v", st, err)
	}
}
