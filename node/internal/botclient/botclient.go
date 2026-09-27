// Package botclient calls the shopper-bot API of spec §9.
package botclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		return &HTTPError{Path: path, Status: res.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("bot %s: %w", path, err)
	}
	return nil
}

// HTTPError is a non-2xx answer of the bot.
type HTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("bot %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

// InProgress tells that the bot is still working on this request_id (409); ask again later (spec §9).
func InProgress(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusConflict
}

// Rejected tells that the bot refused the request itself (4xx other than 409); repeating it does not help.
func Rejected(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status/100 == 4 && he.Status != http.StatusConflict
}

// Purchase is POST /v1/purchase. The bot keeps the result per request_id, so repeating a purchase with the same
// request_id (after a restart or a lost answer) never buys twice (spec §9).
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
