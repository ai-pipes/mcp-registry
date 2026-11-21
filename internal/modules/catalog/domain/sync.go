package domain

import (
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
)

// SyncFailure is why a catalog synchronisation did not happen. The upstream kinds share their
// strings with kernel.UpstreamErrorKind.
type SyncFailure string

// The reasons a synchronisation did not happen.
const (
	FailureNeedsConnection SyncFailure = "needs_connection"
	// FailureCredentialsUnavailable means the admin's OAuth token needed a refresh and the OAuth
	// provider did not answer.
	FailureCredentialsUnavailable SyncFailure = "credentials_unavailable"
	FailureNoSecret               SyncFailure = "no_secret"
	// FailureInternal means an unexpected error inside the synchronisation; the details go to the log only.
	FailureInternal     SyncFailure = "internal"
	FailureUnauthorized SyncFailure = "unauthorized"
	FailureTimeout      SyncFailure = "timeout"
	FailureTooLarge     SyncFailure = "too_large"
	FailureTransport    SyncFailure = "transport"
	FailureRejected     SyncFailure = "rejected"
)

var syncFailureTexts = map[SyncFailure]string{ //nolint:gosec // G101 false positive: failure texts, not credentials
	FailureNeedsConnection:        "An administrator needs to connect this server",
	FailureCredentialsUnavailable: "The OAuth provider is unavailable",
	FailureNoSecret:               "The server secret is not set",
	FailureInternal:               "Internal sync error",
	FailureUnauthorized:           "Upstream rejected the credentials (HTTP 401)",
	FailureTimeout:                "Upstream did not respond in time",
	FailureTooLarge:               "The upstream response exceeds the allowed size",
	FailureTransport:              "Could not reach the upstream",
	FailureRejected:               "Upstream answered tools/list with a JSON-RPC error",
}

// SyncFailureText is the text stored in last_sync_error and shown to admins: no upstream
// details, no secrets. An unknown failure reads as an internal one.
func SyncFailureText(f SyncFailure) string {
	if t, ok := syncFailureTexts[f]; ok {
		return t
	}
	return syncFailureTexts[FailureInternal]
}

// SyncFailureOfUpstream maps an upstream error kind to the failure; an unknown kind is a
// transport failure.
func SyncFailureOfUpstream(k kernel.UpstreamErrorKind) SyncFailure {
	switch k {
	case kernel.UpstreamUnauthorized:
		return FailureUnauthorized
	case kernel.UpstreamTimeout:
		return FailureTimeout
	case kernel.UpstreamTooLarge:
		return FailureTooLarge
	case kernel.UpstreamRejected:
		return FailureRejected
	default:
		return FailureTransport
	}
}

// PersonalCredentialFailure is why a person's own credential to a server could not be had (the
// connections module's answer).
type PersonalCredentialFailure string

// The reasons a personal credential could not be had.
const (
	CredentialNeedsConnection PersonalCredentialFailure = "needs_connection"
	CredentialExpired         PersonalCredentialFailure = "expired"
	CredentialUnavailable     PersonalCredentialFailure = "unavailable"
)

// FailureOfCredential maps the answer of the connections module: a missing or expired connection needs the admin to connect; an OAuth
// provider that could not be reached to renew it is credentials_unavailable, not the admin's to fix.
func FailureOfCredential(f PersonalCredentialFailure) SyncFailure {
	if f == CredentialUnavailable {
		return FailureCredentialsUnavailable
	}
	return FailureNeedsConnection
}

// MayLend reports whether the owner's personal connection is used for a synchronisation only while they are an
// enabled admin (a demoted or disabled admin's connection is not used any more). Nobody (the
// scheduler on a server no admin synchronised) lends nothing.
func MayLend(owner *string, isActiveAdmin bool) bool { return owner != nil && isActiveAdmin }

// SyncRecord is the outcome of one synchronisation attempt, as stored on the server. StartedAt
// orders attempts: an older attempt never overwrites a newer one. SyncedBy is set only by a
// successful run an admin started (it becomes the scheduler's credentials owner).
type SyncRecord struct {
	StartedAt time.Time
	Error     *string
	SyncedBy  *string
}

// SyncSuccess records a successful attempt; startedBy is nil for the scheduler.
func SyncSuccess(startedBy *string, startedAt time.Time) SyncRecord {
	return SyncRecord{StartedAt: startedAt, SyncedBy: copyString(startedBy)}
}

// SyncFailureRecord records a failed attempt with the failure's text; it names nobody.
func SyncFailureRecord(f SyncFailure, startedAt time.Time) SyncRecord {
	text := SyncFailureText(f)
	return SyncRecord{StartedAt: startedAt, Error: &text}
}

func copyString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}
