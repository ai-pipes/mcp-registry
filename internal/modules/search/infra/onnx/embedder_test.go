package onnx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTokenizer records what it is asked to encode and answers with ids of a fixed length.
type fakeTokenizer struct {
	mu     sync.Mutex
	texts  []string
	n      int
	err    error
	closed atomic.Int32
}

func (f *fakeTokenizer) Encode(text string) ([]int64, error) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ids := make([]int64, f.n)
	for i := range ids {
		ids[i] = int64(i%100 + 3)
	}
	if f.n > 0 {
		ids[0], ids[f.n-1] = 0, 2
	}
	return ids, nil
}

func (f *fakeTokenizer) Close() error { f.closed.Add(1); return nil }

func (f *fakeTokenizer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// fakeSession answers every token with the same hidden state, detects overlapping runs and can
// hold a run until released.
type fakeSession struct {
	dim     int
	running atomic.Int32
	overlap atomic.Bool
	gate    chan struct{} // when set, Run waits for it
	entered chan struct{} // when set, Run announces itself
	got     [][]int64
	mu      sync.Mutex
	err     error
	closed  atomic.Int32
}

func (f *fakeSession) Run(ids []int64) ([]float32, error) {
	if f.running.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.running.Add(-1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.got = append(f.got, append([]int64(nil), ids...))
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d := f.dim
	if d == 0 {
		d = Dimensions
	}
	h := make([]float32, len(ids)*d)
	for i := range h {
		h[i] = float32(i%d + 1)
	}
	return h, nil
}

func (f *fakeSession) Close() error { f.closed.Add(1); return nil }

func okLoader(tk *fakeTokenizer, s *fakeSession, calls *atomic.Int32) Loader {
	return func(context.Context) (Backend, error) {
		calls.Add(1)
		return Backend{Tokenizer: tk, Session: s}, nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond) // polling a condition set by another goroutine; there is nothing to block on
	}
}

func TestReadyStartsTheLoadAndNeverBlocksOnIt(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 4}, &fakeSession{}
	e := newEmbedder(func(context.Context) (Backend, error) {
		calls.Add(1)
		<-release
		return Backend{Tokenizer: tk, Session: sess}, nil
	})
	defer func() { _ = e.Close() }()
	if e.Ready(t.Context()) {
		t.Fatal("ready before the model loaded")
	}
	if e.Ready(t.Context()) {
		t.Fatal("ready while the load is still running")
	}
	close(release)
	eventually(t, "Ready", func() bool { return e.Ready(t.Context()) })
	if n := calls.Load(); n != 1 {
		t.Fatalf("the loader ran %d times, want once", n)
	}
}

func TestEmbedWaitsForTheLoadAndLoadsOnce(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 5}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.EmbedPassage(t.Context(), "hello")
			if err != nil || len(v) != Dimensions {
				t.Errorf("EmbedPassage: %d numbers, %v", len(v), err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("the loader ran %d times, want once", n)
	}
}

func TestPrefixesBelongToTheEmbedder(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	if _, err := e.EmbedPassage(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.EmbedQuery(t.Context(), "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(t.Context(), "gamma"); err != nil {
		t.Fatal(err)
	}
	want := []string{"passage: alpha", "query: beta", "gamma"}
	got := tk.seen()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenizer saw %q, want %q", got, want)
		}
	}
}

func TestTheTokenizerSeesTheNormalisedTextWithoutNUL(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	if _, err := e.EmbedQuery(t.Context(), "a\x00b"+string(rune(0xa0))+"c\x01"); err != nil {
		t.Fatal(err)
	}
	if got, want := tk.seen()[0], "query: a\U000F0000b c"; got != want {
		t.Fatalf("tokenizer saw %q, want %q", got, want)
	}
}

func TestALongTextIsCutAt512TokensAndLosesItsClosingToken(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 700}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	res, err := e.EmbedDetailed(t.Context(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.IDs) != MaxTokens || len(sess.got[0]) != MaxTokens {
		t.Fatalf("ids %d, session %d, want %d", len(res.IDs), len(sess.got[0]), MaxTokens)
	}
	if res.IDs[MaxTokens-1] == 2 {
		t.Fatal("</s> is at the end of a cut text")
	}
}

func TestTheVectorIsPooledAndNormalised(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	v, err := e.Embed(t.Context(), "x")
	if err != nil {
		t.Fatal(err)
	}
	// every token has hidden[d] = d+1, so the mean is d+1 and the vector is (d+1)/|(1..384)|.
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum < 0.9999 || sum > 1.0001 {
		t.Fatalf("squared length %v, want 1", sum)
	}
	if v[1]/v[0] < 1.9999 || v[1]/v[0] > 2.0001 {
		t.Fatalf("v[1]/v[0] = %v, want 2", v[1]/v[0])
	}
}

func TestAWrongOutputWidthIsAnError(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{dim: 128}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	if _, err := e.Embed(t.Context(), "x"); err == nil {
		t.Fatal("a 128-wide output was accepted")
	}
}

func TestTokenizerAndSessionFailuresAreErrorsNotPanics(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3, err: errors.New("boom")}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	if _, err := e.Embed(t.Context(), "secret text"); err == nil || strings.Contains(err.Error(), "secret text") {
		t.Fatalf("tokenizer failure: %v", err)
	}
	tk.err = nil
	tk.n = 0
	if _, err := e.Embed(t.Context(), "x"); err == nil {
		t.Fatal("an empty token list was accepted")
	}
	tk.n = 3
	sess.err = errors.New("ort failed")
	if _, err := e.Embed(t.Context(), "x"); err == nil {
		t.Fatal("a session failure was swallowed")
	}
	sess.err = nil
	if _, err := e.Embed(t.Context(), "x"); err != nil {
		t.Fatalf("the embedder did not recover from a run failure: %v", err)
	}
}

func TestAFailedLoadKeepsReadyFalseAndEmbedReturnsTheError(t *testing.T) {
	var calls atomic.Int32
	boom := errors.New("model directory is missing")
	e := newEmbedder(func(context.Context) (Backend, error) { calls.Add(1); return Backend{}, boom })
	defer func() { _ = e.Close() }()
	if e.Ready(t.Context()) {
		t.Fatal("ready after a failed load")
	}
	_, err := e.EmbedPassage(t.Context(), "x")
	if !errors.Is(err, boom) {
		t.Fatalf("Embed returned %v, want the load error", err)
	}
	if e.Ready(t.Context()) {
		t.Fatal("ready after a failed load")
	}
	if err := e.Load(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Load returned %v", err)
	}
}

func TestAFailedLoadIsRetriedOnlyAfterTheCooldown(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(func(context.Context) (Backend, error) {
		calls.Add(1)
		if fail.Load() {
			return Backend{}, errors.New("not yet")
		}
		return Backend{Tokenizer: tk, Session: sess}, nil
	})
	defer func() { _ = e.Close() }()
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	if err := e.Load(t.Context()); err == nil {
		t.Fatal("first load should fail")
	}
	for range 50 { // a storm of calls inside the cooldown
		e.Ready(t.Context())
		if _, err := e.Embed(t.Context(), "x"); err == nil {
			t.Fatal("embedded without a model")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the loader ran %d times inside the cooldown, want 1", n)
	}
	fail.Store(false) // "the model directory was populated afterwards"
	advance(loadRetryAfter + time.Second)
	eventually(t, "the retry to succeed", func() bool { return e.Ready(t.Context()) })
	if _, err := e.Embed(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the loader ran %d times, want 2", n)
	}
}

func TestNotBuiltIsPermanentAndNeverReady(t *testing.T) {
	var calls atomic.Int32
	e := newEmbedder(func(context.Context) (Backend, error) { calls.Add(1); return Backend{}, ErrNotBuilt })
	defer func() { _ = e.Close() }()
	e.now = func() time.Time { return time.Unix(1<<40, 0) } // far past any cooldown
	for range 5 {
		if e.Ready(t.Context()) {
			t.Fatal("ready")
		}
		if _, err := e.EmbedQuery(t.Context(), "x"); !errors.Is(err, ErrNotBuilt) {
			t.Fatalf("err = %v, want ErrNotBuilt", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a permanent failure was retried: %d loads", n)
	}
	if !strings.Contains(ErrNotBuilt.Error(), "not built into this binary") {
		t.Fatalf("the message is %q", ErrNotBuilt)
	}
}

func TestALoaderPanicIsAFailedLoad(t *testing.T) {
	e := newEmbedder(func(context.Context) (Backend, error) { panic("native code blew up") })
	defer func() { _ = e.Close() }()
	if err := e.Load(t.Context()); err == nil {
		t.Fatal("a panicking loader was not reported")
	}
	if e.Ready(t.Context()) {
		t.Fatal("ready")
	}
}

func TestACancelledCallerStopsWaitingButNotTheLoad(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(func(context.Context) (Backend, error) {
		calls.Add(1)
		<-release
		return Backend{Tokenizer: tk, Session: sess}, nil
	})
	defer func() { _ = e.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := e.EmbedPassage(ctx, "x"); done <- err }()
	eventually(t, "the load to start", func() bool { return calls.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	close(release)
	eventually(t, "the load to finish", func() bool { return e.Ready(t.Context()) })
}

func TestCallsAreSerialised(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.EmbedPassage(t.Context(), "x"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if sess.overlap.Load() {
		t.Fatal("two runs overlapped on one session")
	}
	if len(sess.got) != 16 {
		t.Fatalf("%d runs, want 16", len(sess.got))
	}
}

func TestAWaitingCallerHonoursItsContext(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := newEmbedder(okLoader(tk, sess, &calls))
	defer func() { _ = e.Close() }()
	first := make(chan error, 1)
	go func() { _, err := e.EmbedPassage(t.Context(), "x"); first <- err }()
	<-sess.entered // the first run holds the session
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.EmbedPassage(ctx, "y"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	close(sess.gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCloseReleasesBothHandlesOnceAndIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(okLoader(tk, sess, &calls))
	if _, err := e.EmbedPassage(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if tk.closed.Load() != 1 || sess.closed.Load() != 1 {
		t.Fatalf("closed tokenizer %d, session %d, want once each", tk.closed.Load(), sess.closed.Load())
	}
	if e.Ready(t.Context()) {
		t.Fatal("ready after Close")
	}
	if _, err := e.EmbedPassage(t.Context(), "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := e.Load(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Load after Close: %v", err)
	}
}

func TestCloseWaitsForARunInFlight(t *testing.T) {
	var calls atomic.Int32
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := newEmbedder(okLoader(tk, sess, &calls))
	run := make(chan error, 1)
	go func() { _, err := e.EmbedPassage(t.Context(), "x"); run <- err }()
	<-sess.entered
	closed := make(chan struct{})
	go func() { _ = e.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a run was in flight")
	case <-time.After(50 * time.Millisecond): // negative check: nothing to wait on but the absence of an event
	}
	if sess.closed.Load() != 0 {
		t.Fatal("the session was closed under a running call")
	}
	close(sess.gate)
	if err := <-run; err != nil {
		t.Fatalf("the in-flight run failed: %v", err)
	}
	<-closed
	if sess.closed.Load() != 1 {
		t.Fatal("the session was not closed")
	}
}

func TestCloseDuringALoadReleasesWhatTheLoadBuilt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(func(context.Context) (Backend, error) {
		close(started)
		<-release
		return Backend{Tokenizer: tk, Session: sess}, nil
	})
	e.Ready(t.Context())
	<-started
	closed := make(chan struct{})
	go func() { _ = e.Close(); close(closed) }()
	close(release)
	<-closed
	if tk.closed.Load() != 1 || sess.closed.Load() != 1 {
		t.Fatalf("closed tokenizer %d, session %d, want once each", tk.closed.Load(), sess.closed.Load())
	}
	if e.Ready(t.Context()) {
		t.Fatal("ready after Close")
	}
}

func TestCloseWithoutALoadIsFine(t *testing.T) {
	e := newEmbedder(func(context.Context) (Backend, error) { return Backend{}, errors.New("never called") })
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTheRetryPauseDoublesUpToACapAndResetsOnSuccess(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	tk, sess := &fakeTokenizer{n: 3}, &fakeSession{}
	e := newEmbedder(func(context.Context) (Backend, error) {
		calls.Add(1)
		if fail.Load() {
			return Backend{}, errors.New("corrupt model")
		}
		return Backend{Tokenizer: tk, Session: sess}, nil
	})
	defer func() { _ = e.Close() }()
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	e.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	// attempt runs one Embed (which starts a load only when the pause is over) and reports whether
	// the loader ran.
	attempt := func() bool {
		before := calls.Load()
		_, _ = e.Embed(t.Context(), "x")
		return calls.Load() != before
	}

	if !attempt() {
		t.Fatal("the first call must load")
	}
	pause := loadRetryAfter
	for range 12 {
		advance(pause - time.Second)
		if attempt() {
			t.Fatalf("retried before the pause of %v was over", pause)
		}
		advance(time.Second)
		if !attempt() {
			t.Fatalf("did not retry after the pause of %v", pause)
		}
		pause = min(2*pause, loadRetryMax)
	}
	if pause != loadRetryMax {
		t.Fatalf("the pause never reached the cap: %v", pause)
	}
	// An explicit Load tries at once, inside the pause.
	before := calls.Load()
	if err := e.Load(t.Context()); err == nil || calls.Load() != before+1 {
		t.Fatalf("Load inside the pause: err %v, loader runs %d", err, calls.Load()-before)
	}
	// Success resets the pause: after a later failure it starts at the first value again.
	fail.Store(false)
	advance(loadRetryMax)
	if err := e.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.retryAfter != 0 {
		t.Fatalf("the pause is %v after a success, want 0", e.retryAfter)
	}
}

func TestTheFirstPauseIsFiveSeconds(t *testing.T) {
	e := newEmbedder(func(context.Context) (Backend, error) { return Backend{}, errors.New("no") })
	defer func() { _ = e.Close() }()
	if err := e.Load(t.Context()); err == nil {
		t.Fatal("expected a failure")
	}
	if e.retryAfter != 5*time.Second {
		t.Fatalf("first pause %v, want 5s", e.retryAfter)
	}
}
