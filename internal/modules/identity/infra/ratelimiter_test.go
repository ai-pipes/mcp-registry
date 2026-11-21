package infra

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock { return &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) set(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
}

func (c *stepClock) add(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Duration(ms) * time.Millisecond)
}

func expectHit(t *testing.T, l *Limiter, key string, allowed bool, retry int) {
	t.Helper()
	a, r := l.Hit(key)
	if a != allowed || r != retry {
		t.Fatalf("Hit(%q) = (%v, %d), want (%v, %d)", key, a, r, allowed, retry)
	}
}

func expectCheck(t *testing.T, l *Limiter, key string, allowed bool, retry int) {
	t.Helper()
	a, r := l.Check(key)
	if a != allowed || r != retry {
		t.Fatalf("Check(%q) = (%v, %d), want (%v, %d)", key, a, r, allowed, retry)
	}
}

func TestLimiterAllowsLimitHitsThenLocks(t *testing.T) {
	c := newStepClock()
	l := NewLimiter(3, 60, 120, c)
	for i := 0; i < 3; i++ {
		expectHit(t, l, "k", true, 0)
	}
	expectHit(t, l, "k", false, 120)
	c.set(60_000) // window passed but still locked
	expectHit(t, l, "k", false, 60)
	c.set(120_001)
	a, _ := l.Hit("k")
	if !a {
		t.Fatal("lock should have ended")
	}
}

func TestLimiterRemainingSecondsRoundUp(t *testing.T) {
	c := newStepClock()
	l := NewLimiter(0, 60, 10, c)
	l.Hit("k")
	c.set(9_001)
	expectCheck(t, l, "k", false, 1)
	c.set(10_000)
	expectCheck(t, l, "k", true, 0)
}

func TestLimiterKeysAreIndependentAndResetClears(t *testing.T) {
	l := NewLimiter(1, 60, 60, newStepClock())
	expectHit(t, l, "a", true, 0)
	expectHit(t, l, "b", true, 0)
	if a, _ := l.Hit("a"); a {
		t.Fatal("a should be locked")
	}
	l.Reset("a")
	expectHit(t, l, "a", true, 0)
}

func TestLimiterWindowSlidesForwardWhenIdle(t *testing.T) {
	c := newStepClock()
	l := NewLimiter(2, 10, 10, c)
	l.Hit("k")
	l.Hit("k")
	c.set(10_000)
	expectHit(t, l, "k", true, 0)
}

func TestLimiterCheckDoesNotCountAndReportsLock(t *testing.T) {
	l := NewLimiter(1, 60, 60, newStepClock())
	for i := 0; i < 5; i++ {
		expectCheck(t, l, "k", true, 0)
	}
	l.Hit("k")
	l.Hit("k")
	expectCheck(t, l, "k", false, 60)
}

func TestLimiterIsBoundedAndEvictsOldest(t *testing.T) {
	l := newLimiter(1, 60, 60, newStepClock(), 100, 50000)
	for i := 0; i < 110; i++ {
		l.Hit(fmt.Sprintf("k%d", i))
	}
	if l.Size() != 100 {
		t.Fatalf("size = %d, want 100", l.Size())
	}
	l.Hit("k109")
	if a, _ := l.Hit("k109"); a {
		t.Fatal("k109 should be at its limit")
	}
	if a, _ := l.Hit("k0"); !a {
		t.Fatal("k0 was evicted and must start fresh")
	}
}

func TestLimiterSweepsAtMostOncePerWindow(t *testing.T) {
	c := newStepClock()
	l := newLimiter(1, 10, 1, c, 1000, 1000)
	for i := 0; i < 500; i++ {
		l.Hit(fmt.Sprintf("k%d", i))
	}
	if l.Sweeps() != 0 {
		t.Fatalf("sweeps = %d, want 0", l.Sweeps())
	}
	c.set(10_000)
	for i := 0; i < 500; i++ {
		l.Hit(fmt.Sprintf("n%d", i))
	}
	if l.Sweeps() != 1 {
		t.Fatalf("sweeps = %d, want 1", l.Sweeps())
	}
	if l.Size() != 500 {
		t.Fatalf("size = %d, want 500 (stale k* swept)", l.Size())
	}
}

func TestLimiterLocksSurviveAFloodOfNonLockingKeys(t *testing.T) {
	l := newLimiter(1, 60, 600, newStepClock(), 100, 50000)
	l.Hit("victim")
	l.Hit("victim")
	expectCheck(t, l, "victim", false, 600)
	for i := 0; i < 1000; i++ {
		l.Hit(fmt.Sprintf("flood%d", i))
	}
	if l.Size() != 100 {
		t.Fatalf("size = %d, want 100", l.Size())
	}
	if a, _ := l.Check("victim"); a {
		t.Fatal("the lock must survive the flood")
	}
}

func TestLimiterFullLockMapEvictsEarliestLock(t *testing.T) {
	c := newStepClock()
	l := newLimiter(0, 60, 600, c, 100, 3)
	for _, k := range []string{"a", "b", "c", "d"} {
		l.Hit(k)
		c.add(1000)
	}
	if l.LockedSize() != 3 {
		t.Fatalf("locked = %d, want 3", l.LockedSize())
	}
	expectCheck(t, l, "a", true, 0)
	for _, k := range []string{"b", "c", "d"} {
		if a, _ := l.Check(k); a {
			t.Fatalf("%s should be locked", k)
		}
	}
}

func TestLimiterExpiredLocksAreSweptAndRelockWorks(t *testing.T) {
	c := newStepClock()
	l := newLimiter(0, 10, 10, c, 100, 3)
	l.Hit("a")
	c.set(11_000)
	l.Hit("b")
	if l.LockedSize() != 1 {
		t.Fatalf("locked = %d, want 1", l.LockedSize())
	}
	l.Hit("a")
	if a, _ := l.Check("a"); a {
		t.Fatal("a should be locked again")
	}
}

func TestLimiterResetClearsCountersAndLocks(t *testing.T) {
	l := NewLimiter(0, 60, 60, newStepClock())
	l.Hit("k")
	if a, _ := l.Check("k"); a {
		t.Fatal("locked expected")
	}
	l.Reset("k")
	expectCheck(t, l, "k", true, 0)
}

func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l := NewLimiter(5, 60, 60, newStepClock())
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if a, _ := l.Hit("shared"); a {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
				l.Check("shared")
			}
		}()
	}
	wg.Wait()
	if allowed != 5 {
		t.Fatalf("allowed = %d, want exactly the limit 5", allowed)
	}
}
