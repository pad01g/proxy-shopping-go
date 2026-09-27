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
	running map[string]bool
	wake    chan struct{}
	started bool
}

func newDispatcher(m *Messenger) *dispatcher {
	return &dispatcher{m: m, queues: map[string][]*Message{}, running: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func dispatchKey(msg *Message) string {
	if msg.OrderID != "" {
		return "o:" + msg.OrderID
	}
	return "p:" + msg.From
}

func (d *dispatcher) start(ctx context.Context) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()
	for i := 0; i < max(Workers, 1); i++ {
		go d.work(ctx)
	}
}

func (d *dispatcher) enqueue(msg *Message) {
	k := dispatchKey(msg)
	d.mu.Lock()
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

func (d *dispatcher) done(k string) {
	d.mu.Lock()
	delete(d.running, k)
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
			return // left pending in the store, handled at the next start
		}
		d.m.handle(ctx, msg)
		d.done(k)
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

const maxBuckets = 10000

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

// sweep forgets buckets that are full again (their senders were quiet long enough).
func (r *rateLimiter) sweep(now time.Time) {
	for k, b := range r.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*r.perMin >= r.burst {
			delete(r.buckets, k)
		}
	}
}
