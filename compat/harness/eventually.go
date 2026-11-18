//go:build compat

package harness

import (
	"crypto/rand"
	"encoding/hex"
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

func eventuallyWithin(t testing.TB, what string, limit time.Duration, f func() bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(pollStep)
	defer tick.Stop()
	for {
		if f() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out after %s waiting for %s", limit, what)
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
