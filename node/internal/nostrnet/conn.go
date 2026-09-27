package nostrnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
)

// A small NIP-01 client. go-nostr's Relay (v0.52.3) races on its own fields whenever a connection closes
// (relay.go: the writer goroutine clears Connection and reads ConnectionError while close() and the reader set
// them), so connections are kept here: one reader goroutine, writes under a mutex, state behind c.mu.

const (
	maxMessageSize = 4 << 20
	pingInterval   = 29 * time.Second
	subBuffer      = 64
)

var errClosed = errors.New("connection closed")

type okResult struct {
	ok     bool
	reason string
}

type conn struct {
	url    string
	ws     *websocket.Conn
	log    *slog.Logger
	ctx    context.Context // ends when the connection is gone
	cancel context.CancelCauseFunc

	writeMu sync.Mutex

	mu       sync.Mutex
	subs     map[string]*subscription
	oks      map[string]chan okResult
	lastUsed time.Time

	nextSub atomic.Int64
}

func dial(ctx context.Context, url string, hc *http.Client, log *slog.Logger) (*conn, error) {
	ws, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc, CompressionMode: websocket.CompressionContextTakeover})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessageSize)
	cctx, cancel := context.WithCancelCause(context.Background())
	c := &conn{
		url: url, ws: ws, log: log, ctx: cctx, cancel: cancel,
		subs: map[string]*subscription{}, oks: map[string]chan okResult{}, lastUsed: time.Now(),
	}
	go c.readLoop()
	go c.pingLoop()
	return c, nil
}

func (c *conn) alive() bool { return c.ctx.Err() == nil }

// close ends the connection and every subscription on it. It is safe to call more than once and concurrently.
func (c *conn) close(cause error) {
	if c.ctx.Err() != nil {
		return
	}
	c.cancel(cause)
	_ = c.ws.CloseNow()
	c.mu.Lock()
	subs := c.subs
	c.subs = map[string]*subscription{}
	c.mu.Unlock()
	for _, s := range subs {
		s.end(cause)
	}
}

func (c *conn) touch() {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// idleSince returns when the connection was last used, or zero while subscriptions are open.
func (c *conn) idleSince() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subs) > 0 || len(c.oks) > 0 {
		return time.Time{}
	}
	return c.lastUsed
}

func (c *conn) write(ctx context.Context, msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.alive() {
		return errClosed
	}
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		c.close(fmt.Errorf("write: %w", err))
		return err
	}
	return nil
}

func (c *conn) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		err := c.ws.Ping(ctx)
		cancel()
		if err != nil {
			c.close(fmt.Errorf("ping: %w", err))
			return
		}
	}
}

func (c *conn) readLoop() {
	parser := nostr.NewMessageParser()
	var buf bytes.Buffer
	for {
		buf.Reset()
		_, r, err := c.ws.Reader(c.ctx)
		if err == nil {
			_, err = io.Copy(&buf, r)
		}
		if err != nil {
			c.close(fmt.Errorf("read: %w", err))
			return
		}
		env, err := parser.ParseMessage(buf.String())
		if env == nil || err != nil {
			continue
		}
		switch env := env.(type) {
		case *nostr.EventEnvelope:
			if env.SubscriptionID == nil {
				continue
			}
			c.mu.Lock()
			s := c.subs[*env.SubscriptionID]
			c.mu.Unlock()
			if s == nil || !s.filters.Match(&env.Event) {
				continue
			}
			// the id must be the hash of the event: a relay could otherwise send any validly signed event
			// under the id of another one, and a receiver remembering ids as seen would drop the real one
			if !env.Event.CheckID() {
				c.log.Debug("dropping event with a wrong id", "relay", c.url, "id", env.Event.ID)
				continue
			}
			if ok, _ := env.Event.CheckSignature(); !ok {
				c.log.Debug("dropping event with a bad signature", "relay", c.url, "id", env.Event.ID)
				continue
			}
			ev := env.Event
			s.deliver(&ev)
		case *nostr.EOSEEnvelope:
			c.mu.Lock()
			s := c.subs[string(*env)]
			c.mu.Unlock()
			if s != nil {
				s.eoseOnce.Do(func() { close(s.eose) })
			}
		case *nostr.ClosedEnvelope:
			c.mu.Lock()
			s := c.subs[env.SubscriptionID]
			delete(c.subs, env.SubscriptionID)
			c.mu.Unlock()
			if s != nil {
				s.end(fmt.Errorf("closed by relay: %s", env.Reason))
			}
		case *nostr.OKEnvelope:
			c.mu.Lock()
			ch := c.oks[env.EventID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- okResult{env.OK, env.Reason}:
				default:
				}
			}
		case *nostr.NoticeEnvelope:
			c.log.Debug("relay notice", "relay", c.url, "notice", string(*env))
		}
	}
}

// publish sends an event and waits for the relay's OK.
func (c *conn) publish(ctx context.Context, ev *nostr.Event) error {
	c.touch()
	ch := make(chan okResult, 1)
	c.mu.Lock()
	c.oks[ev.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.oks, ev.ID)
		c.lastUsed = time.Now()
		c.mu.Unlock()
	}()
	msg, err := nostr.EventEnvelope{Event: *ev}.MarshalJSON()
	if err != nil {
		return err
	}
	if err := c.write(ctx, msg); err != nil {
		return err
	}
	select {
	case res := <-ch:
		if !res.ok {
			return fmt.Errorf("msg: %s", res.reason)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
}

// subscription is one REQ on a connection.
type subscription struct {
	c       *conn
	id      string
	filters nostr.Filters
	events  chan *nostr.Event
	eose    chan struct{}
	done    chan struct{}

	eoseOnce sync.Once
	endOnce  sync.Once
	err      error // set before done closes
}

func (s *subscription) deliver(ev *nostr.Event) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *subscription) end(err error) {
	s.endOnce.Do(func() {
		s.err = err
		close(s.done)
	})
}

// unsub sends CLOSE (best effort) and ends the subscription.
func (s *subscription) unsub() {
	c := s.c
	c.mu.Lock()
	_, open := c.subs[s.id]
	delete(c.subs, s.id)
	c.lastUsed = time.Now()
	c.mu.Unlock()
	if open && c.alive() {
		msg, _ := nostr.CloseEnvelope(s.id).MarshalJSON()
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.write(wctx, msg)
		cancel()
	}
	s.end(errors.New("unsubscribed"))
}

func (c *conn) subscribe(ctx context.Context, filters nostr.Filters) (*subscription, error) {
	s := &subscription{
		c: c, id: "ps" + strconv.FormatInt(c.nextSub.Add(1), 10), filters: filters,
		events: make(chan *nostr.Event, subBuffer), eose: make(chan struct{}), done: make(chan struct{}),
	}
	c.mu.Lock()
	c.subs[s.id] = s
	c.lastUsed = time.Now()
	c.mu.Unlock()
	msg, err := nostr.ReqEnvelope{SubscriptionID: s.id, Filters: filters}.MarshalJSON()
	if err == nil {
		err = c.write(ctx, msg)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.subs, s.id)
		c.mu.Unlock()
		s.end(err)
		return nil, err
	}
	return s, nil
}

// query collects the stored events of a filter until EOSE, without duplicates and at most the filter's limit
// (MaxQueryEvents without one): a relay cannot make us hold an unbounded answer.
func (c *conn) query(ctx context.Context, filter nostr.Filter) ([]*nostr.Event, error) {
	max := MaxQueryEvents
	if filter.Limit > 0 && filter.Limit < max {
		max = filter.Limit
	}
	s, err := c.subscribe(ctx, nostr.Filters{filter})
	if err != nil {
		return nil, err
	}
	defer s.unsub()
	var out []*nostr.Event
	seen := map[string]bool{}
	add := func(ev *nostr.Event) bool {
		if !seen[ev.ID] {
			seen[ev.ID] = true
			out = append(out, ev)
		}
		return len(out) >= max
	}
	for {
		select {
		case ev := <-s.events:
			if add(ev) {
				return out, nil
			}
		case <-s.eose:
			// the reader may have queued events just before EOSE
			for {
				select {
				case ev := <-s.events:
					if add(ev) {
						return out, nil
					}
				default:
					return out, nil
				}
			}
		case <-s.done:
			return out, s.err
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}
