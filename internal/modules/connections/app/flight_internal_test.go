package app

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingleFlightJoinsCallsWithTheSameKey(t *testing.T) {
	f := NewSingleFlight[int]()
	var runs atomic.Int32
	release := make(chan struct{})
	job := func(context.Context) (int, error) {
		<-release
		return int(runs.Add(1)), nil
	}
	type out struct {
		v   int
		err error
	}
	ch := make(chan out, 3)
	for _, key := range []string{"k", "k", "other"} {
		go func() {
			v, err := f.Do(context.Background(), key, job)
			ch <- out{v, err}
		}()
	}
	waitForJoined(t, f, "k", 2)
	waitForJoined(t, f, "other", 1)
	close(release)
	got := map[int]int{}
	for range 3 {
		o := <-ch
		if o.err != nil {
			t.Fatal(o.err)
		}
		got[o.v]++
	}
	if runs.Load() != 2 {
		t.Fatalf("ran %d times, want 2 (one per key)", runs.Load())
	}
	// "k" callers got one shared value: some value appears twice.
	twice := false
	for _, n := range got {
		twice = twice || n == 2
	}
	if !twice {
		t.Fatalf("the two callers of k did not share a result: %v", got)
	}
	// Once settled, the same key runs again.
	v, err := f.Do(context.Background(), "k", func(context.Context) (int, error) { return 99, nil })
	if err != nil || v != 99 {
		t.Fatalf("%v %v", v, err)
	}
}

// waitForJoined waits (without sleeping) until n callers wait on key.
func waitForJoined[T any](t *testing.T, f *SingleFlight[T], key string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.joined(key) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d callers joined %q", f.joined(key), key)
		}
		runtime.Gosched()
	}
}

func TestSingleFlightAJoinerGetsTheFirstCallersResult(t *testing.T) {
	f := NewSingleFlight[string]()
	release := make(chan struct{})
	first := make(chan string, 1)
	go func() {
		v, _ := f.Do(context.Background(), "k", func(context.Context) (string, error) {
			<-release
			return "first", nil
		})
		first <- v
	}()
	second := make(chan string, 1)
	waitForJoined(t, f, "k", 1)
	go func() {
		v, _ := f.Do(context.Background(), "k", func(context.Context) (string, error) { return "second", nil })
		second <- v
	}()
	waitForJoined(t, f, "k", 2)
	close(release)
	if v := <-second; v != "first" {
		t.Fatalf("the joiner got %q", v)
	}
	if v := <-first; v != "first" {
		t.Fatalf("the first caller got %q", v)
	}
}

func TestSingleFlightErrorsAreSharedAndThenForgotten(t *testing.T) {
	f := NewSingleFlight[int]()
	boom := errors.New("x")
	if _, err := f.Do(context.Background(), "k", func(context.Context) (int, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	v, err := f.Do(context.Background(), "k", func(context.Context) (int, error) { return 4, nil })
	if err != nil || v != 4 {
		t.Fatalf("%v %v", v, err)
	}
}

func TestSingleFlightACancelledWaiterReturnsWhileTheWorkContinues(t *testing.T) {
	f := NewSingleFlight[int]()
	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	work := func(c context.Context) (int, error) {
		close(started)
		select {
		case <-release:
		case <-c.Done():
			return 0, c.Err()
		}
		close(finished)
		return 7, nil
	}
	cctx, cancel := context.WithCancel(context.Background())
	creator := make(chan error, 1)
	go func() {
		_, err := f.Do(cctx, "k", work)
		creator <- err
	}()
	<-started
	joiner := make(chan int, 1)
	go func() {
		v, _ := f.Do(context.Background(), "k", work)
		joiner <- v
	}()
	waitForJoined(t, f, "k", 2)
	cancel()
	if err := <-creator; !errors.Is(err, context.Canceled) {
		t.Fatalf("the creator returned %v", err)
	}
	select {
	case <-finished:
		t.Fatal("the work ended with the creator's cancellation")
	default:
	}
	close(release)
	if v := <-joiner; v != 7 {
		t.Fatalf("the joiner got %d", v)
	}
}

func TestSingleFlightWorkIsBoundedAndKeepsValues(t *testing.T) {
	f := NewSingleFlight[string]()
	f.bound = 20 * time.Millisecond
	type k struct{}
	parent := context.WithValue(context.Background(), k{}, "v")
	v, err := f.Do(parent, "k", func(c context.Context) (string, error) {
		<-c.Done() // only the bound ends it: the caller's context is never cancelled here
		got, _ := c.Value(k{}).(string)
		return got + "/" + c.Err().Error(), nil
	})
	if err != nil || v != "v/context deadline exceeded" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestSingleFlightAPanicBecomesAnErrorWithoutItsText(t *testing.T) {
	f := NewSingleFlight[int]()
	_, err := f.Do(context.Background(), "k", func(context.Context) (int, error) { panic("TOKENMARK") })
	if err == nil || strings.Contains(err.Error(), "TOKENMARK") {
		t.Fatalf("%v", err)
	}
	// The key is free again.
	if v, err := f.Do(context.Background(), "k", func(context.Context) (int, error) { return 1, nil }); err != nil || v != 1 {
		t.Fatalf("%v %v", v, err)
	}
}
