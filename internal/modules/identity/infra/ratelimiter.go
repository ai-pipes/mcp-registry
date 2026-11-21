package infra

import (
	"container/list"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

const defaultMaxKeys = 50_000

// ordered is a map that remembers insertion order, so the oldest key can be evicted.
type ordered[V any] struct {
	items map[string]*list.Element
	order *list.List
}

type entry[V any] struct {
	key string
	val V
}

func newOrdered[V any]() *ordered[V] {
	return &ordered[V]{items: map[string]*list.Element{}, order: list.New()}
}

func (o *ordered[V]) get(k string) (V, bool) {
	if e, ok := o.items[k]; ok {
		return e.Value.(*entry[V]).val, true
	}
	var zero V
	return zero, false
}

func (o *ordered[V]) delete(k string) {
	if e, ok := o.items[k]; ok {
		o.order.Remove(e)
		delete(o.items, k)
	}
}

// set inserts at the end; an existing key is moved there.
func (o *ordered[V]) set(k string, v V) {
	o.delete(k)
	o.items[k] = o.order.PushBack(&entry[V]{key: k, val: v})
}

func (o *ordered[V]) evictFirst() {
	if f := o.order.Front(); f != nil {
		o.delete(f.Value.(*entry[V]).key)
	}
}

func (o *ordered[V]) len() int { return len(o.items) }

type counter struct {
	count       int
	windowStart time.Time
}

// Limiter is a fixed-window counter with a lock-out: the (limit+1)-th hit inside a window locks the
// key for lockSec. Every hit counts. Counters and locks live in separate maps, each capped, so a
// flood of non-locking keys can only evict other counters, never a lock. Every lock lasts the same
// time and is inserted now, so the locks map's insertion order is its expiry order and the first
// key is the one evicted when it is full. Stale entries are swept at most once per window.
type Limiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	lock      time.Duration
	clock     kernel.Clock
	maxKeys   int
	maxLocks  int
	counters  *ordered[*counter]
	locks     *ordered[time.Time]
	lastSweep time.Time
	sweeps    int
}

var _ app.RateLimiter = (*Limiter)(nil)

// NewLimiter returns a limiter capped at 50 000 counters and 50 000 locks.
func NewLimiter(limit, windowSec, lockSec int, clock kernel.Clock) *Limiter {
	return newLimiter(limit, windowSec, lockSec, clock, defaultMaxKeys, defaultMaxKeys)
}

func newLimiter(limit, windowSec, lockSec int, clock kernel.Clock, maxKeys, maxLocks int) *Limiter {
	return &Limiter{
		limit: limit, window: time.Duration(windowSec) * time.Second, lock: time.Duration(lockSec) * time.Second,
		clock: clock, maxKeys: maxKeys, maxLocks: maxLocks,
		counters: newOrdered[*counter](), locks: newOrdered[time.Time](), lastSweep: clock.Now(),
	}
}

// Size is the number of counting entries.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counters.len()
}

// LockedSize is the number of locked keys.
func (l *Limiter) LockedSize() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locks.len()
}

// Sweeps is the number of sweeps performed (observability, tests).
func (l *Limiter) Sweeps() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweeps
}

// Check does not count: is the key locked out?
func (l *Limiter) Check(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(key, l.clock.Now())
}

func (l *Limiter) check(key string, now time.Time) (bool, int) {
	if until, ok := l.locks.get(key); ok && until.After(now) {
		return false, ceilSeconds(until.Sub(now))
	}
	return true, 0
}

func ceilSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// Hit counts one failure. While the key is locked a hit does not count and returns the remaining seconds.
func (l *Limiter) Hit(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if now.Sub(l.lastSweep) >= l.window {
		l.sweep(now)
	}
	if ok, retry := l.check(key, now); !ok {
		return false, retry
	}
	c, ok := l.counters.get(key)
	if !ok || now.Sub(c.windowStart) >= l.window {
		l.counters.delete(key)
		for l.counters.len() >= l.maxKeys {
			l.counters.evictFirst()
		}
		c = &counter{windowStart: now}
		l.counters.set(key, c)
	}
	c.count++
	if c.count <= l.limit {
		return true, 0
	}
	l.counters.delete(key)
	l.locks.delete(key)
	for l.locks.len() >= l.maxLocks {
		l.locks.evictFirst()
	}
	l.locks.set(key, now.Add(l.lock))
	return false, int(l.lock / time.Second)
}

// Reset forgets a key: its counter and its lock.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counters.delete(key)
	l.locks.delete(key)
}

func (l *Limiter) sweep(now time.Time) {
	l.lastSweep = now
	l.sweeps++
	for e := l.counters.order.Front(); e != nil; {
		next := e.Next()
		if en := e.Value.(*entry[*counter]); now.Sub(en.val.windowStart) >= l.window {
			l.counters.delete(en.key)
		}
		e = next
	}
	for e := l.locks.order.Front(); e != nil; {
		next := e.Next()
		if en := e.Value.(*entry[time.Time]); !en.val.After(now) {
			l.locks.delete(en.key)
		}
		e = next
	}
}
