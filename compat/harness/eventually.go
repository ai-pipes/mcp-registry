//go:build compat

package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

const (
	pollStep        = 100 * time.Millisecond
	eventuallyLimit = 15 * time.Second
	readyLimit      = 120 * time.Second
)

// Eventually polls f every 100 ms until it returns true; the test fails after 15 s.
func Eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	eventuallyWithin(t, what, eventuallyLimit, f)
}

// EventuallyWithin is Eventually with another limit, for a wait whose bound the host's load decides
// rather than the behaviour under test (the outbox delivery of a whole catalog on a busy machine).
func EventuallyWithin(t *testing.T, what string, limit time.Duration, f func() bool) {
	t.Helper()
	eventuallyWithin(t, what, limit, f)
}

func eventuallyWithin(t testing.TB, what string, limit time.Duration, f func() bool) {
	t.Helper()
	if err := waitFor(what, limit, func() (bool, error) { return f(), nil }); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls f every 100 ms until it reports done, returns an error, or the limit passes.
func waitFor(what string, limit time.Duration, f func() (bool, error)) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(pollStep)
	defer tick.Stop()
	for {
		done, err := f()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if done {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for %s", limit, what)
		case <-tick.C:
		}
	}
}

// Unique returns prefix plus a random suffix: slugs and subs that do not collide on a shared stack.
func Unique(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source failing is not recoverable
	}
	return prefix + "-" + hex.EncodeToString(b)
}
