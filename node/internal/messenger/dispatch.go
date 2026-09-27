package messenger

import (
	"context"
	"sync"
	"time"
)

// dispatcher runs handlers on a bounded set of workers. Messages with the same key (the order id, or the sender
// for messages without one) are handled one after the other in arrival order; different keys run in parallel,
// so one slow order does not hold up the others.
type dispatcher struct {
	m *Messenger

	mu      sync.Mutex
	queues  map[string][]*Message // waiting messages per key
	ready   []string              // keys with waiting messages and no running handler, in order
	running map[string]bool       // keys whose handler runs (possibly past its timeout)
	ids     map[string]bool       // inner ids queued or running, so that one message is never in twice
	wake    chan struct{}
	ctx     context.Context // of the running workers; nil before the first start
}

func newDispatcher(m *Messenger) *dispatcher {
	return &dispatcher{m: m, queues: map[string][]*Message{}, running: map[string]bool{}, ids: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func dispatchKey(msg *Message) string {
	if msg.OrderID != "" {
		return "o:" + msg.OrderID
	}
	return "p:" + msg.From
}

// start runs the workers until ctx ends. Once the workers of an earlier start stopped, start runs new ones: the
// messages still queued then are forgotten (they are pending in the store and queued again by redispatch), while
// handlers still running keep their keys busy.
func (d *dispatcher) start(ctx context.Context) {
	d.mu.Lock()
	if d.ctx != nil && d.ctx.Err() == nil {
		d.mu.Unlock()
		return
	}
	if d.ctx != nil {
		for _, q := range d.queues {
			for _, msg := range q {
				delete(d.ids, msg.Inner.ID)
			}
		}
		d.queues, d.ready = map[string][]*Message{}, nil
	}
	d.ctx = ctx
	d.mu.Unlock()
	for i := 0; i < max(Workers, 1); i++ {
		go d.work(ctx)
	}
}

func (d *dispatcher) enqueue(msg *Message) {
	k := dispatchKey(msg)
	d.mu.Lock()
	if d.ids[msg.Inner.ID] {
		d.mu.Unlock()
		return
	}
	d.ids[msg.Inner.ID] = true
	if len(d.queues[k]) == 0 && !d.running[k] {
		d.ready = append(d.ready, k)
	}
	d.queues[k] = append(d.queues[k], msg)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// next takes the first message of the first ready key and marks the key running.
func (d *dispatcher) next() (string, *Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ready) == 0 {
		return "", nil
	}
	k := d.ready[0]
	d.ready = d.ready[1:]
	q := d.queues[k]
	msg := q[0]
	if len(q) == 1 {
		delete(d.queues, k)
	} else {
		d.queues[k] = q[1:]
	}
	d.running[k] = true
	if len(d.ready) > 0 {
		// more work: let another worker pick it up
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	return k, msg
}

func (d *dispatcher) done(k string, msg *Message) {
	d.mu.Lock()
	delete(d.running, k)
	delete(d.ids, msg.Inner.ID)
	if len(d.queues[k]) > 0 {
		d.ready = append(d.ready, k)
	}
	more := len(d.ready) > 0
	d.mu.Unlock()
	if more {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
}

func (d *dispatcher) work(ctx context.Context) {
	for {
		k, msg := d.next()
		if msg == nil {
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
				continue
			}
		}
		if ctx.Err() != nil {
			d.done(k, msg) // left pending in the store, handled at the next start
			return
		}
		if wait := d.m.handle(ctx, msg); wait != nil {
			// the handler overran its timeout: its order stays busy until it returns, this worker goes on
			go func(k string, msg *Message) {
				<-wait
				d.done(k, msg)
			}(k, msg)
			continue
		}
		d.done(k, msg)
	}
}

// rateLimiter is a token bucket per sender.
type rateLimiter struct {
	perMin, burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

var maxBuckets = 10000

func newRateLimiter(perMin, burst int) *rateLimiter {
	return &rateLimiter{perMin: float64(perMin), burst: float64(burst), buckets: map[string]*bucket{}}
}

func (r *rateLimiter) allow(key string, now time.Time) bool {
	if r.perMin <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.buckets[key]
	if b == nil {
		if len(r.buckets) >= maxBuckets {
			r.sweep(now)
		}
		if len(r.buckets) >= maxBuckets {
			r.evictOldest()
		}
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Minutes()*r.perMin)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldest forgets the bucket used longest ago, so that the map never exceeds maxBuckets.
func (r *rateLimiter) evictOldest() {
	var oldest string
	var at time.Time
	found := false
	for k, b := range r.buckets {
		if !found || b.last.Before(at) {
			oldest, at, found = k, b.last, true
		}
	}
	delete(r.buckets, oldest)
}

func (r *rateLimiter) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// sweep forgets buckets that are full again (their senders were quiet long enough).
func (r *rateLimiter) sweep(now time.Time) {
	for k, b := range r.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*r.perMin >= r.burst {
			delete(r.buckets, k)
		}
	}
}
