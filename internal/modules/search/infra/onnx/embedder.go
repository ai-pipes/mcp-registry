package onnx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// Dimensions is the length of every vector (multilingual-e5-small).
	Dimensions = 384
	// ModelID is the model's directory below the model root, as in the TypeScript server
	// (<MODEL_DIR>/Xenova/multilingual-e5-small).
	ModelID = "Xenova/multilingual-e5-small"
	// PassagePrefix is what the model expects in front of a document; the Embedder adds it, the
	// callers pass the bare text.
	PassagePrefix = "passage: "
	// QueryPrefix is the same for a search query.
	QueryPrefix = "query: "

	// loadRetryAfter is how long a failed load is remembered. Inside it Ready and the Embed calls
	// answer with the old error without touching the files again; after it the next call tries
	// again (the TypeScript embedder retries on every call; this bounds the cost of a storm).
	loadRetryAfter = 5 * time.Second
)

var (
	// ErrNotBuilt is the failure of the build without the onnx tag. It is permanent: never retried.
	ErrNotBuilt = errors.New("the local embedder is not built into this binary")
	// ErrClosed is returned by every call after Close.
	ErrClosed = errors.New("the local embedder is closed")

	errLoading = errors.New("the local embedder is still loading")
)

// Tokenizer turns a prepared text (see PrepareText) into token ids WITH the special tokens
// (<s> ... </s>) and without any cut; the Embedder truncates. Implementations need not be safe for
// concurrent use: the Embedder runs one call at a time.
type Tokenizer interface {
	Encode(text string) ([]int64, error)
	Close() error
}

// Session runs the model on one text: ids is the token list (attention_mask all ones, token_type_ids
// all zeros are the session's business) and the result is last_hidden_state, len(ids)*Dimensions
// values, row-major.
type Session interface {
	Run(ids []int64) ([]float32, error)
	Close() error
}

// Backend is what a load produces.
type Backend struct {
	Tokenizer Tokenizer
	Session   Session
}

// Loader builds a Backend. It runs at most once at a time and its context is never cancelled by a
// caller giving up. A failure leaves nothing open.
type Loader func(ctx context.Context) (Backend, error)

// Embedder is the local embedder. Safe for concurrent use; calls to the model are serialised (one
// text per run, one run at a time). The model is loaded lazily: Ready starts the load, Embed waits
// for it, Load blocks on it (the composition loads before serving).
type Embedder struct {
	loader     Loader
	now        func() time.Time
	retryAfter time.Duration

	runSem   chan struct{} // capacity 1: one run at a time
	closedCh chan struct{} // closed by Close

	mu       sync.Mutex
	backend  *Backend
	call     *loadCall
	lastErr  error
	failedAt time.Time
	closed   bool
}

type loadCall struct {
	done chan struct{}
	err  error
}

func newEmbedder(loader Loader) *Embedder {
	return &Embedder{
		loader:     loader,
		now:        time.Now,
		retryAfter: loadRetryAfter,
		runSem:     make(chan struct{}, 1),
		closedCh:   make(chan struct{}),
	}
}

// ensure makes the model loaded. With wait false it never blocks: it starts a load when none runs
// and the last failure is old enough, and answers errLoading while one is in flight.
func (e *Embedder) ensure(ctx context.Context, wait bool) error {
	e.mu.Lock()
	switch {
	case e.closed:
		e.mu.Unlock()
		return ErrClosed
	case e.backend != nil:
		e.mu.Unlock()
		return nil
	}
	call := e.call
	if call == nil {
		if e.lastErr != nil && (errors.Is(e.lastErr, ErrNotBuilt) || e.now().Sub(e.failedAt) < e.retryAfter) {
			err := e.lastErr
			e.mu.Unlock()
			return err
		}
		call = &loadCall{done: make(chan struct{})}
		e.call = call
		go e.runLoad(context.WithoutCancel(ctx), call)
	}
	e.mu.Unlock()
	if !wait {
		return errLoading
	}
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Embedder) runLoad(ctx context.Context, call *loadCall) {
	backend, err := e.safeLoad(ctx)
	e.mu.Lock()
	e.call = nil
	if err == nil && e.closed {
		err = ErrClosed
		_ = closeBackend(backend)
	}
	switch {
	case err == nil:
		e.backend = &backend
		e.lastErr = nil
	case !errors.Is(err, ErrClosed):
		e.lastErr = err
		e.failedAt = e.now()
	}
	call.err = err
	e.mu.Unlock()
	close(call.done)
}

// safeLoad turns a panic in the loader (native code) into a failed load.
func (e *Embedder) safeLoad(ctx context.Context) (b Backend, err error) {
	defer func() {
		if recover() != nil {
			b, err = Backend{}, errors.New("loading the local embedder panicked")
		}
	}()
	return e.loader(ctx)
}

// Ready reports whether the model is usable now. It never blocks on a load; it starts one when none
// is running (and retries a failed one after a short pause). A load failure is not reported here:
// Ready keeps answering false and Embed and Load return the error.
func (e *Embedder) Ready(ctx context.Context) bool {
	return e.ensure(ctx, false) == nil
}

// Load blocks until the model is loaded and returns the load error, if any. The composition calls
// it before serving. After a failure it does not retry inside the pause (see loadRetryAfter).
func (e *Embedder) Load(ctx context.Context) error {
	return e.ensure(ctx, true)
}

// EmbedPassage embeds "passage: <text>". It waits for the model to load and returns Dimensions
// numbers or an error.
func (e *Embedder) EmbedPassage(ctx context.Context, text string) ([]float32, error) {
	return e.Embed(ctx, PassagePrefix+text)
}

// EmbedQuery embeds "query: <text>".
func (e *Embedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return e.Embed(ctx, QueryPrefix+text)
}

// Embed embeds a text as given (the prefix, if any, is the caller's).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	res, err := e.EmbedDetailed(ctx, text)
	return res.Vector, err
}

// Result is a vector with the token ids that produced it (for diagnostics and the parity tools).
type Result struct {
	IDs    []int64
	Vector []float32
}

// EmbedDetailed is Embed that also returns the (truncated) token ids.
func (e *Embedder) EmbedDetailed(ctx context.Context, text string) (Result, error) {
	if err := e.ensure(ctx, true); err != nil {
		return Result{}, err
	}
	select {
	case e.runSem <- struct{}{}:
	case <-e.closedCh:
		return Result{}, ErrClosed
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-e.runSem }()
	e.mu.Lock()
	backend := e.backend
	e.mu.Unlock()
	if backend == nil {
		return Result{}, ErrClosed
	}
	return embedOne(backend, text)
}

func embedOne(b *Backend, text string) (Result, error) {
	ids, err := b.Tokenizer.Encode(PrepareText(text))
	if err != nil {
		return Result{}, fmt.Errorf("tokenizing failed: %w", err)
	}
	if len(ids) == 0 {
		return Result{}, errors.New("the tokenizer returned no tokens")
	}
	ids = truncate(ids)
	hidden, err := b.Session.Run(ids)
	if err != nil {
		return Result{}, fmt.Errorf("running the model failed: %w", err)
	}
	if len(hidden) != len(ids)*Dimensions {
		return Result{}, fmt.Errorf("the model returned %d numbers for %d tokens, want %d each", len(hidden), len(ids), Dimensions)
	}
	v := meanPool(hidden, len(ids), Dimensions)
	l2Normalize(v)
	return Result{IDs: ids, Vector: v}, nil
}

// Close releases the tokenizer and the session. It waits for a load and for a run in flight; every
// call after it returns ErrClosed. Safe to call twice.
func (e *Embedder) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.closedCh)
	call := e.call
	e.mu.Unlock()
	if call != nil {
		<-call.done // runLoad closes what it built
	}
	e.runSem <- struct{}{} // no run is in flight any more, and none can start
	e.mu.Lock()
	backend := e.backend
	e.backend = nil
	e.mu.Unlock()
	if backend == nil {
		return nil
	}
	return closeBackend(*backend)
}

func closeBackend(b Backend) error {
	var errs []error
	if b.Tokenizer != nil {
		errs = append(errs, b.Tokenizer.Close())
	}
	if b.Session != nil {
		errs = append(errs, b.Session.Close())
	}
	return errors.Join(errs...)
}
