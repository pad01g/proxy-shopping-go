// Package botclient calls the shopper-bot API of spec §9.
package botclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// Client talks to one bot.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a client; purchases can take minutes, so the timeout is generous.
func New(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("bot %s: %w", path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("bot %s: %w", path, err)
	}
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("bot %s: HTTP %d: %s", path, res.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("bot %s: %w", path, err)
	}
	return nil
}

// Purchase is POST /v1/purchase.
func (c *Client) Purchase(ctx context.Context, r proto.PurchaseRequest) (*proto.PurchaseResult, error) {
	var out proto.PurchaseResult
	if err := c.post(ctx, "/v1/purchase", r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Tracking is POST /v1/tracking.
func (c *Client) Tracking(ctx context.Context, q proto.TrackingQuery) (*proto.TrackingStatus, error) {
	var out proto.TrackingStatus
	if err := c.post(ctx, "/v1/tracking", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
