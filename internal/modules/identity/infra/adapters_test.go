package infra

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
)

// NewPg sets the three limiters up with the TypeScript numbers: token checks 30 per 60 s (lock
// 60 s), link attempts 10 per 60 s (lock 60 s), failed callbacks 20 per 60 s (lock 60 s). Each
// is driven to its limit through the port with a fixed clock, so a changed number fails here.
func TestNewPgLimiterNumbers(t *testing.T) {
	clock := newStepClock()
	a := NewPg(nil, clock)
	cases := []struct {
		name  string
		l     app.RateLimiter
		limit int
	}{
		{"token checks", a.TokenLimiter, 30},
		{"link attempts", a.LinkLimiter, 10},
		{"failed callbacks", a.CallbackLimiter, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock.set(0)
			for i := 1; i <= c.limit; i++ {
				if ok, _ := c.l.Hit("k"); !ok {
					t.Fatalf("hit %d of %d refused", i, c.limit)
				}
			}
			if ok, retry := c.l.Hit("k"); ok || retry != 60 {
				t.Fatalf("hit %d = (%v, %d), want refused with a 60 s lock", c.limit+1, ok, retry)
			}
			clock.set(59_000)
			if ok, retry := c.l.Check("k"); ok || retry != 1 {
				t.Fatalf("at 59 s: Check = (%v, %d), want locked for 1 s more", ok, retry)
			}
			clock.set(61_000)
			if ok, _ := c.l.Check("k"); !ok {
				t.Fatal("still locked after the 60 s lock")
			}
			// The window is 60 s: a key that stayed at its limit is counted afresh after it.
			clock.set(200_000)
			for i := 1; i <= c.limit; i++ {
				if ok, _ := c.l.Hit("w"); !ok {
					t.Fatalf("window key: hit %d refused", i)
				}
			}
			clock.set(262_000)
			for i := 1; i <= c.limit; i++ {
				if ok, _ := c.l.Hit("w"); !ok {
					t.Fatalf("window key after 62 s: hit %d refused (window longer than 60 s)", i)
				}
			}
		})
	}
	if a.TokenLimiter == a.LinkLimiter || a.LinkLimiter == a.CallbackLimiter || a.TokenLimiter == a.CallbackLimiter {
		t.Fatal("two of the limiters are one instance")
	}
}
