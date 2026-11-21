package domain_test

import (
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const hour = time.Hour

func TestSessionLifetime(t *testing.T) {
	if got := domain.SessionExpiry(t0).Sub(t0); got != 30*24*hour {
		t.Fatalf("absolute limit = %v", got)
	}
	exp := domain.SessionExpiry(t0)
	cases := []struct {
		name     string
		lastSeen time.Time
		expires  time.Time
		now      time.Time
		alive    bool
	}{
		{"exactly 12h idle is alive (<=)", t0, exp, t0.Add(12 * hour), true},
		{"12h idle plus 1ns is dead", t0, exp, t0.Add(12*hour + 1), false},
		{"at the absolute limit is dead (<)", exp.Add(-time.Second), exp, exp, false},
		{"1ns before the absolute limit is alive", exp.Add(-time.Second), exp, exp.Add(-1), true},
		{"fresh", t0, exp, t0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.IsSessionAlive(c.lastSeen, c.expires, c.now); got != c.alive {
				t.Fatalf("alive = %v, want %v", got, c.alive)
			}
		})
	}
}

func TestDeadSessionCutoffsAgreeWithIsSessionAlive(t *testing.T) {
	now := t0.Add(40 * 24 * hour)
	c := domain.DeadSessionCutoffs(now)
	if !c.ExpiresAtOrBefore.Equal(now) || !c.LastSeenBefore.Equal(now.Add(-12*hour)) {
		t.Fatalf("cutoffs = %+v", c)
	}
	dead := func(lastSeen, expires time.Time) bool {
		return !expires.After(c.ExpiresAtOrBefore) || lastSeen.Before(c.LastSeenBefore)
	}
	cases := [][2]time.Time{
		{now.Add(-12 * hour), now.Add(1)},
		{now.Add(-12*hour - 1), now.Add(hour)},
		{now.Add(-1), now},
		{now.Add(-1), now.Add(1)},
	}
	for i, s := range cases {
		if dead(s[0], s[1]) == domain.IsSessionAlive(s[0], s[1], now) {
			t.Errorf("case %d: SQL-side dead=%v but alive=%v", i, dead(s[0], s[1]), domain.IsSessionAlive(s[0], s[1], now))
		}
	}
}
