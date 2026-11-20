//go:build compat

package harness

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// pausedPosts sends one POST, waits pause on an idle connection, and sends a second POST with the
// same client; it returns the transport error of the second one (nil when it was answered). The
// pause is the deliberate, documented purpose of the tests below: they need the server's keep-alive
// timeout (Node's default is 5 s) to run out while the connection sits idle, there is no event to
// wait on. The wait is bounded by ctx.
func pausedPosts(ctx context.Context, c *http.Client, url string, pause time.Duration) error {
	post := func() error {
		// POST /api/auth/logout without a session answers 204 and changes nothing.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/auth/logout", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Origin", url)
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if err := post(); err != nil {
		return err
	}
	select {
	case <-time.After(pause):
	case <-ctx.Done():
		return ctx.Err()
	}
	return post()
}

// sweep runs pausedPosts concurrently, one client per pause in [5 s - spread, 5 s + spread], and
// returns the number of failures and the first error. The window in which a request meets a
// connection the server is closing is a few milliseconds wide, so the pauses are spread over it.
func sweep(ctx context.Context, newClient func() *http.Client, url string, spread, step time.Duration) (failed, total int, first error) {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for off := -spread; off <= spread; off += step {
		wg.Add(1)
		go func(pause time.Duration) {
			defer wg.Done()
			err := pausedPosts(ctx, newClient(), url, pause)
			mu.Lock()
			defer mu.Unlock()
			total++
			if err != nil {
				failed++
				if first == nil {
					first = err
				}
			}
		}(5*time.Second + off)
	}
	wg.Wait()
	return failed, total, first
}

// TestIdleConnectionsOutliveNoServerKeepAlive: the harness's clients must not reuse a connection
// that has been idle for as long as a server's keep-alive timeout, or a POST that meets the
// closing connection dies with EOF. With the harness transport (IdleConnTimeout one second) the
// second POST after a pause of about 5 s never fails.
//
// COMPAT_REPRO_KEEPALIVE=<rounds> additionally runs the same sweep with a transport WITHOUT an idle
// timeout, and logs how many second requests failed: it documents the failure mode the timeout
// removes and is not an assertion (whether a request meets the closing connection is a race). It
// is off by default because each round takes about 5 s.
func TestIdleConnectionsOutliveNoServerKeepAlive(t *testing.T) {
	s := Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The timeout is part of the harness client; assert it where it is set.
	if got := newHTTPClient(nil).Transport.(*http.Transport).IdleConnTimeout; got != idleConnTimeout || got <= 0 || got > 2*time.Second {
		t.Fatalf("the harness transport's IdleConnTimeout is %s", got)
	}
	failed, total, first := sweep(ctx, func() *http.Client { return newHTTPClient(nil) }, s.loopbackURL(), 100*time.Millisecond, 25*time.Millisecond)
	if failed != 0 {
		t.Fatalf("with the harness transport %d of %d second POSTs after an idle pause failed: %v", failed, total, first)
	}

	rounds, _ := strconv.Atoi(os.Getenv("COMPAT_REPRO_KEEPALIVE"))
	for round := 1; round <= rounds; round++ {
		failed, total, first := sweep(ctx, func() *http.Client {
			return &http.Client{Transport: newTransport(0), Timeout: 30 * time.Second}
		}, s.loopbackURL(), 100*time.Millisecond, 10*time.Millisecond)
		t.Logf("repro round %d, no idle timeout: %d of %d second POSTs failed (first: %v)", round, failed, total, first)
	}
}

// stalledPosts is pausedPosts with an event-loop stall around the keep-alive deadline: the app
// container is paused for the moment the server's 5 s idle timer runs out and the second request
// is written while the process is frozen, so the process wakes up to an expired idle timer and an
// already queued request on the same connection (the situation of a busy host, made
// deterministic). The pauses are the deliberate purpose of the test (see pausedPosts).
func (s *Stack) stalledPosts(ctx context.Context, c *http.Client) error {
	post := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.loopbackURL()+"/api/auth/logout", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Origin", s.loopbackURL())
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := post(); err != nil {
		return err
	}
	if err := wait(4500 * time.Millisecond); err != nil {
		return err
	}
	if _, err := dockerErr("pause", s.app); err != nil {
		return err
	}
	unpaused := false
	defer func() {
		if !unpaused {
			_, _ = dockerErr("unpause", s.app)
		}
	}()
	if err := wait(time.Second); err != nil { // idle for 5.5 s now; the server's timer is overdue
		return err
	}
	result := make(chan error, 1)
	go func() { result <- post() }()
	if err := wait(500 * time.Millisecond); err != nil {
		return err
	}
	if _, err := dockerErr("unpause", s.app); err != nil {
		return err
	}
	unpaused = true
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestAStalledAppDoesNotKillAPostOnAnIdleConnection: with the app frozen across its keep-alive
// deadline, the harness transport still gets its second POST answered. COMPAT_REPRO_KEEPALIVE
// (any value) also runs a transport without an idle timeout and logs what happens to it.
func TestAStalledAppDoesNotKillAPostOnAnIdleConnection(t *testing.T) {
	s := Shared(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.stalledPosts(ctx, newHTTPClient(nil)); err != nil {
		t.Fatalf("the second POST with the harness transport: %v", err)
	}
	if os.Getenv("COMPAT_REPRO_KEEPALIVE") != "" {
		err := s.stalledPosts(ctx, &http.Client{Transport: newTransport(0), Timeout: 30 * time.Second})
		t.Logf("repro, no idle timeout, app frozen across the keep-alive deadline: second POST error: %v", err)
	}
}
