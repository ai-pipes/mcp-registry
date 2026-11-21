package domain

import "time"

const (
	// SessionIdleTTL is how long a session may go unused.
	SessionIdleTTL = 12 * time.Hour
	// SessionAbsoluteTTL is the hard limit of a session, however active.
	SessionAbsoluteTTL = 30 * 24 * time.Hour
)

// SessionExpiry is the absolute end of a session created at now.
func SessionExpiry(now time.Time) time.Time { return now.Add(SessionAbsoluteTTL) }

// IsSessionAlive reports whether a session is still usable: it ends at its absolute limit (now < expiresAt) or after 12 h without use
// (now <= lastSeenAt + 12 h).
func IsSessionAlive(lastSeenAt, expiresAt, now time.Time) bool {
	return now.Before(expiresAt) && !now.After(lastSeenAt.Add(SessionIdleTTL))
}

// SessionCutoffs is where IsSessionAlive ends, as cutoffs a store can delete by: a session is dead
// once its expiry is at or before ExpiresAtOrBefore, or it was last seen before LastSeenBefore.
type SessionCutoffs struct {
	ExpiresAtOrBefore time.Time
	LastSeenBefore    time.Time
}

// DeadSessionCutoffs computes the cutoffs for now.
func DeadSessionCutoffs(now time.Time) SessionCutoffs {
	return SessionCutoffs{ExpiresAtOrBefore: now, LastSeenBefore: now.Add(-SessionIdleTTL)}
}
