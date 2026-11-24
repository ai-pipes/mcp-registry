package domain

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

const pendingConnectTTL = 10 * time.Minute

// PendingConnect is a started OAuth connect: one-time, short-lived, bound to the person who started
// it and to the server configuration it started with. State and the code verifier are secrets
// (kernel.Secret): printing a PendingConnect shows neither, however it is held.
type PendingConnect struct {
	state        kernel.Secret
	userID       string
	server       string
	codeVerifier kernel.Secret
	issuedFor    string
	expiresAt    time.Time
}

// PendingSnapshot is the stored form of a PendingConnect, for rehydration.
type PendingSnapshot struct {
	State        kernel.Secret
	UserID       string
	Server       string
	CodeVerifier kernel.Secret
	IssuedFor    string
	ExpiresAt    time.Time
}

// NewPendingConnect starts an attempt at now; it expires ten minutes later. issuedFor is the
// ServerFingerprint when it started.
func NewPendingConnect(state kernel.Secret, userID, server string, codeVerifier kernel.Secret, issuedFor string, now time.Time) PendingConnect {
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
func (p PendingConnect) State() kernel.Secret { return p.state }

// UserID is the person who started the attempt.
func (p PendingConnect) UserID() string { return p.userID }

// Server is the slug of the server being connected.
func (p PendingConnect) Server() string { return p.server }

// CodeVerifier is the PKCE verifier.
func (p PendingConnect) CodeVerifier() kernel.Secret { return p.codeVerifier }

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

func (p PendingConnect) safe() string {
	return fmt.Sprintf("PendingConnect{server:%s expiresAt:%s}", p.server, p.expiresAt.Format(time.RFC3339))
}

// Format prints the server and expiry only, for every verb, for the value and a pointer.
func (p PendingConnect) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, p.safe()) }

// LogValue is the slog form: server and expiry only.
func (p PendingConnect) LogValue() slog.Value { return slog.StringValue(p.safe()) }

// MarshalJSON writes server and expiry only.
func (p PendingConnect) MarshalJSON() ([]byte, error) { return json.Marshal(p.safe()) }

// MarshalText writes server and expiry only.
func (p PendingConnect) MarshalText() ([]byte, error) { return []byte(p.safe()), nil }

func (s PendingSnapshot) safe() string {
	return fmt.Sprintf("PendingSnapshot{server:%s expiresAt:%s}", s.Server, s.ExpiresAt.Format(time.RFC3339))
}

// Format prints the server and expiry only: State and CodeVerifier are secrets.
func (s PendingSnapshot) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, s.safe()) }

// LogValue is the slog form: server and expiry only.
func (s PendingSnapshot) LogValue() slog.Value { return slog.StringValue(s.safe()) }

// MarshalJSON writes server and expiry only.
func (s PendingSnapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.safe()) }

// MarshalText writes server and expiry only.
func (s PendingSnapshot) MarshalText() ([]byte, error) { return []byte(s.safe()), nil }
