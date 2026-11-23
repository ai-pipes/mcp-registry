package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

// flightBound limits the detached work of one flight: the provider's own 10 s timeout, the wait for
// the row lock behind another process's refresh (up to the same again) and the database round trips.
const flightBound = 30 * time.Second

// errFlightPanic is what the waiters get when the shared work panicked. The panic value is not
// kept: it could quote a token. The package has no logger (Deps carries none), so nothing is
// logged here: the error reaches every waiter, and the caller's own error path (the REST layer's
// 500 handler, the gateway's internal-error text, the sync's failure record) logs it, with the
// fixed text below as the only thing to look for.
var errFlightPanic = errors.New("connections: credential renewal panicked")

type flightCall[T any] struct {
	done    chan struct{}
	val     T
	err     error
	waiters int
}

// SingleFlight joins concurrent calls with the same key into one run inside this process. Callers
// that arrive while a run is under way wait for its result, whatever their own arguments are (a
// caller with forceRefresh joining a renewal that was not forced gets that renewal's result, as in
// the TypeScript reference); once it settles the key is free again.
//
// The run is detached from the callers: it takes the first caller's context values but not its
// cancellation, bounded by flightBound. A caller whose context ends returns at once with the
// context's error and the work goes on for the others (a provider that has used up a one-time
// refresh token must get its answer stored). Build it with NewSingleFlight.
//
// When the 30 s bound elapses the work's context ends and every waiter receives whatever fn returns
// for an ended context: for a resolve the provider call fails as "no answer" (the unexpired
// access token is used, else FailureUnavailable) or, when the bound ends while waiting for the row
// lock, the store's error (an unexpected error, not a CredentialFailure). Waiters are not cut off
// separately: they return when fn does, or when their own context ends.
type SingleFlight[T any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[T]
	bound time.Duration
}

// NewSingleFlight makes an empty SingleFlight.
func NewSingleFlight[T any]() *SingleFlight[T] {
	return &SingleFlight[T]{calls: map[string]*flightCall[T]{}, bound: flightBound}
}

// Do runs fn once for concurrent calls with the same key and returns its result to all of them.
// fn gets a context that outlives ctx's cancellation.
func (f *SingleFlight[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	f.mu.Lock()
	c, running := f.calls[key]
	if !running {
		c = &flightCall[T]{done: make(chan struct{})}
		f.calls[key] = c
		go f.run(context.WithoutCancel(ctx), key, c, fn)
	}
	c.waiters++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		c.waiters--
		f.mu.Unlock()
	}()

	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (f *SingleFlight[T]) run(ctx context.Context, key string, c *flightCall[T], fn func(context.Context) (T, error)) {
	defer func() {
		if recover() != nil {
			var zero T
			c.val, c.err = zero, errFlightPanic
		}
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		close(c.done)
	}()
	ctx, cancel := context.WithTimeout(ctx, f.bound)
	defer cancel()
	c.val, c.err = fn(ctx)
}

// joined is the number of callers waiting on key now (the one that started the run included).
func (f *SingleFlight[T]) joined(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.calls[key]; ok {
		return c.waiters
	}
	return 0
}
