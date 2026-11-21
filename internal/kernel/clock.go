package kernel

import "time"

// Clock is the source of the current time.
type Clock interface{ Now() time.Time }

// SystemClock reads the wall clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock always returns T; for tests.
type FixedClock struct{ T time.Time }

// Now returns T.
func (c FixedClock) Now() time.Time { return c.T }
