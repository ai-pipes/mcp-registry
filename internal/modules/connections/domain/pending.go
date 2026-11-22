package domain

import (
	"fmt"
	"time"
)

const pendingConnectTTL = 10 * time.Minute

// PendingConnect is a started OAuth connect: one-time, short-lived, bound to the person who started
// it and to the server configuration it started with. State and the code verifier are secrets:
// printing a PendingConnect shows neither.
type PendingConnect struct {
	state        string
	userID       string
	server       string
	codeVerifier string
	issuedFor    string
	expiresAt    time.Time
}

// PendingSnapshot is the stored form of a PendingConnect, for rehydration.
type PendingSnapshot struct {
	State        string
	UserID       string
	Server       string
	CodeVerifier string
	IssuedFor    string
	ExpiresAt    time.Time
}

// NewPendingConnect starts an attempt at now; it expires ten minutes later. issuedFor is the
// ServerFingerprint when it started.
func NewPendingConnect(state, userID, server, codeVerifier, issuedFor string, now time.Time) PendingConnect {
	return PendingConnect{
		state: state, userID: userID, server: server, codeVerifier: codeVerifier,
		issuedFor: issuedFor, expiresAt: PendingConnectExpiry(now),
	}
}

// RestorePendingConnect rebuilds a stored attempt without validation.
func RestorePendingConnect(s PendingSnapshot) PendingConnect {
	return PendingConnect{
		state: s.State, userID: s.UserID, server: s.Server, codeVerifier: s.CodeVerifier,
		issuedFor: s.IssuedFor, expiresAt: s.ExpiresAt,
	}
}

// PendingConnectExpiry is when an attempt started at now stops being valid.
func PendingConnectExpiry(now time.Time) time.Time { return now.Add(pendingConnectTTL) }

// IsPendingConnectExpired reports whether an attempt expiring at expiresAt is over at now: at
// exactly expiresAt it is.
func IsPendingConnectExpired(expiresAt, now time.Time) bool { return !now.Before(expiresAt) }

// State is the one-time state parameter.
func (p PendingConnect) State() string { return p.state }

// UserID is the person who started the attempt.
func (p PendingConnect) UserID() string { return p.userID }

// Server is the slug of the server being connected.
func (p PendingConnect) Server() string { return p.server }

// CodeVerifier is the PKCE verifier.
func (p PendingConnect) CodeVerifier() string { return p.codeVerifier }

// IssuedFor is the ServerFingerprint the attempt started with.
func (p PendingConnect) IssuedFor() string { return p.issuedFor }

// ExpiresAt is when the attempt stops being valid.
func (p PendingConnect) ExpiresAt() time.Time { return p.expiresAt }

// CanComplete reports whether userID may finish the attempt at now: the starter, before expiry.
func (p PendingConnect) CanComplete(userID string, now time.Time) bool {
	return p.userID == userID && !IsPendingConnectExpired(p.expiresAt, now)
}

// StartedWithConfig reports whether config (a ServerFingerprint) is still the one the attempt
// started with: when an admin changed the provider meanwhile, the code is not sent to the new one.
func (p PendingConnect) StartedWithConfig(config string) bool { return p.issuedFor == config }

// Format prints the server and expiry only, for every verb and for both the value and a pointer.
func (p PendingConnect) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "PendingConnect{server:%s expiresAt:%s}", p.server, p.expiresAt.Format(time.RFC3339))
}
