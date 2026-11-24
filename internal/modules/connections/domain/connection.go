package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Kind is the kind of a personal credential.
type Kind string

// Credential kinds.
const (
	KindOAuth Kind = "oauth"
	KindPAT   Kind = "pat"
)

// Status is the state of a connection. No stored row means needs_connection (listed as missing).
type Status string

// Connection statuses.
const (
	StatusReady   Status = "ready"
	StatusExpired Status = "expired"
)

// ExpiryReason is why a ready connection turns expired (the audit meta.reason values).
type ExpiryReason string

// Expiry reasons.
const (
	ReasonTokenExpired     ExpiryReason = "token_expired"
	ReasonConfigChanged    ExpiryReason = "config_changed"
	ReasonRefreshRejected  ExpiryReason = "refresh_rejected"
	ReasonRejectedUpstream ExpiryReason = "rejected_upstream"
)

// PlanAction is what to do with a connection when its secret is needed (a closed set).
type PlanAction string

// Plan actions.
const (
	// PlanUse hands out the current secret.
	PlanUse PlanAction = "use"
	// PlanRefresh gets a new access token with the refresh token first.
	PlanRefresh PlanAction = "refresh"
	// PlanExpire: the token is unusable and cannot be refreshed; mark the connection expired.
	PlanExpire PlanAction = "expire"
	// PlanExpired: already expired; only a new connect helps.
	PlanExpired PlanAction = "expired"
)

// CredentialPlan is the outcome of Connection.CredentialPlan. RefreshToken is set only for
// PlanRefresh, Reason only for PlanExpire.
type CredentialPlan struct {
	Action       PlanAction
	RefreshToken Sealed
	Reason       ExpiryReason
}

// PlanOptions are the inputs of CredentialPlan besides the time. Config is the ServerFingerprint of
// the server's configuration now.
type PlanOptions struct {
	ForceRefresh bool
	Config       string
}

// ProviderDownOptions are the inputs of UsableWhileProviderDown.
type ProviderDownOptions struct{ ForceRefresh bool }

// refreshAhead: refresh this long before the access token runs out.
const refreshAhead = 60 * time.Second

// RefreshFailure is how the token endpoint answered a refresh: its HTTP status and OAuth error
// code ("" when the answer carried none). A nil *RefreshFailure means there was no answer at all
// (timeout, DNS, connection refused, malformed answer).
type RefreshFailure struct {
	Status int
	Error  string
}

// IsRefreshRejected reports whether a failed refresh means the person's grant is gone: the token
// endpoint answered with an OAuth error (invalid_grant, or HTTP 400/401). No answer, a timeout, a
// network error or another 5xx says nothing about the grant. The gateway's own client credentials
// failing (invalid_client, or unauthorized_client with 401) is the admin's to fix and is not a
// refusal of the grant either; unauthorized_client with 400 (Gitea's unparseable refresh token) is.
func IsRefreshRejected(f *RefreshFailure) bool {
	if f == nil {
		return false
	}
	if f.Error == "invalid_client" {
		return false
	}
	if f.Error == "unauthorized_client" && f.Status == 401 {
		return false
	}
	return f.Error == "invalid_grant" || f.Status == 400 || f.Status == 401
}

// SealedTokens are token material sealed outside the domain. A Refresh of length zero means none;
// a nil ExpiresAt means no known expiry.
type SealedTokens struct {
	Access    Sealed
	Refresh   Sealed
	ExpiresAt *time.Time
}

// Snapshot is the stored form of a Connection. Refresh of length zero means none; nil RevokeURL
// and ExpiresAt mean none.
type Snapshot struct {
	UserID string
	Server string
	// IssuedFor is the ServerFingerprint of the server configuration the credential was issued for.
	IssuedFor string
	// RevokeURL is the OAuth revoke URL in place when the tokens were issued (nil: none then, or a
	// PAT). On disconnect the token goes only to this URL.
	RevokeURL *string
	Kind      Kind
	Status    Status
	Access    Sealed
	Refresh   Sealed
	ExpiresAt *time.Time
	UpdatedAt time.Time
}

// OAuthConnect is the input of ConnectOAuth.
type OAuthConnect struct {
	UserID    string
	Server    string
	Tokens    SealedTokens
	IssuedFor string
	RevokeURL *string
	Now       time.Time
}

// PATConnect is the input of ConnectPAT.
type PATConnect struct {
	UserID    string
	Server    string
	Token     Sealed
	IssuedFor string
	Now       time.Time
}

// Connection is a person's credentials to one server: an OAuth token pair or a PAT. Tokens are held
// only sealed; the aggregate never sees plain text.
type Connection struct{ s Snapshot }

// ConnectOAuth is a new ready OAuth connection.
func ConnectOAuth(in OAuthConnect) *Connection {
	return &Connection{s: Snapshot{
		UserID: in.UserID, Server: in.Server, IssuedFor: in.IssuedFor, RevokeURL: cloneStr(in.RevokeURL),
		Kind: KindOAuth, Status: StatusReady, Access: cloneSealed(in.Tokens.Access),
		Refresh: cloneSealed(in.Tokens.Refresh), ExpiresAt: cloneTime(in.Tokens.ExpiresAt), UpdatedAt: in.Now,
	}}
}

// ConnectPAT is a new ready PAT connection: no expiry the gateway knows of and nothing to refresh it with.
func ConnectPAT(in PATConnect) *Connection {
	return &Connection{s: Snapshot{
		UserID: in.UserID, Server: in.Server, IssuedFor: in.IssuedFor, Kind: KindPAT, Status: StatusReady,
		Access: cloneSealed(in.Token), UpdatedAt: in.Now,
	}}
}

// Restore rebuilds a stored connection without validation.
func Restore(s Snapshot) *Connection { return &Connection{s: copySnapshot(s)} }

func cloneStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copySnapshot(s Snapshot) Snapshot {
	s.RevokeURL = cloneStr(s.RevokeURL)
	s.Access = cloneSealed(s.Access)
	s.Refresh = cloneSealed(s.Refresh)
	s.ExpiresAt = cloneTime(s.ExpiresAt)
	return s
}

// UserID is the person.
func (c *Connection) UserID() string { return c.s.UserID }

// Server is the server slug.
func (c *Connection) Server() string { return c.s.Server }

// Kind is oauth or pat.
func (c *Connection) Kind() Kind { return c.s.Kind }

// Status is ready or expired (not config-aware: see StatusFor).
func (c *Connection) Status() Status { return c.s.Status }

// AccessToken is the sealed access token (or PAT), a copy.
func (c *Connection) AccessToken() Sealed { return cloneSealed(c.s.Access) }

// RefreshToken is the sealed refresh token, a copy; empty when there is none.
func (c *Connection) RefreshToken() Sealed { return cloneSealed(c.s.Refresh) }

// ExpiresAt is the access token's expiry, a copy; nil when unknown.
func (c *Connection) ExpiresAt() *time.Time { return cloneTime(c.s.ExpiresAt) }

// UpdatedAt is when the credential last changed.
func (c *Connection) UpdatedAt() time.Time { return c.s.UpdatedAt }

// RevokeURL is the revoke URL in place at issue time, a copy; nil when none.
func (c *Connection) RevokeURL() *string { return cloneStr(c.s.RevokeURL) }

// Snapshot is the stored form, a deep copy.
func (c *Connection) Snapshot() Snapshot { return copySnapshot(c.s) }

func (c Connection) safe() string { return c.s.safe() }

func (s Snapshot) safe() string {
	return fmt.Sprintf("Snapshot{user:%s server:%s kind:%s status:%s}", s.UserID, s.Server, s.Kind, s.Status)
}

// Format prints identifiers and state only, never token material. The value receiver covers a
// Connection value, a pointer, slice elements and embedding structs alike.
func (c Connection) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.safe()) }

// LogValue is the slog form: identifiers and state only.
func (c Connection) LogValue() slog.Value { return slog.StringValue(c.safe()) }

// MarshalJSON writes identifiers and state only.
func (c Connection) MarshalJSON() ([]byte, error) { return json.Marshal(c.safe()) }

// MarshalText writes identifiers and state only.
func (c Connection) MarshalText() ([]byte, error) { return []byte(c.safe()), nil }

// Format prints identifiers and state only, never token material.
func (s Snapshot) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.safe()) }

// LogValue is the slog form: identifiers and state only.
func (s Snapshot) LogValue() slog.Value { return slog.StringValue(s.safe()) }

// MarshalJSON writes identifiers and state only.
func (s Snapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.safe()) }

// MarshalText writes identifiers and state only.
func (s Snapshot) MarshalText() ([]byte, error) { return []byte(s.safe()), nil }

// CredentialPlan decides what to do when the secret is needed. An expired connection stays expired
// until the person connects again; a credential issued for another server configuration never
// leaves (it expires); a PAT is used as is (when upstream rejects it the gateway marks it expired);
// an OAuth token is refreshed ahead of time (less than 60 s left) or when upstream rejected it
// (ForceRefresh); without a refresh token it is used while it lasts.
func (c *Connection) CredentialPlan(now time.Time, o PlanOptions) CredentialPlan {
	if c.s.Status == StatusExpired {
		return CredentialPlan{Action: PlanExpired}
	}
	if !c.IssuedFor(o.Config) {
		return CredentialPlan{Action: PlanExpire, Reason: ReasonConfigChanged}
	}
	if c.s.Kind == KindPAT {
		return CredentialPlan{Action: PlanUse}
	}
	// left is unbounded when there is no expiry.
	hasExpiry := c.s.ExpiresAt != nil
	var left time.Duration
	if hasExpiry {
		left = c.s.ExpiresAt.Sub(now)
	}
	if !o.ForceRefresh && (!hasExpiry || left >= refreshAhead) {
		return CredentialPlan{Action: PlanUse}
	}
	if !c.s.Refresh.IsEmpty() {
		return CredentialPlan{Action: PlanRefresh, RefreshToken: cloneSealed(c.s.Refresh)}
	}
	if !o.ForceRefresh && (!hasExpiry || left > 0) {
		return CredentialPlan{Action: PlanUse}
	}
	return CredentialPlan{Action: PlanExpire, Reason: ReasonTokenExpired}
}

// UsableWhileProviderDown reports, for a refresh that could not reach the provider, whether the
// current access token may still be handed out while it has not run out, unless upstream just rejected it (ForceRefresh).
func (c *Connection) UsableWhileProviderDown(now time.Time, o ProviderDownOptions) bool {
	if c.s.Status == StatusExpired || o.ForceRefresh {
		return false
	}
	return c.s.ExpiresAt == nil || now.Before(*c.s.ExpiresAt)
}

// IssuedFor reports whether the credential was issued for the server configuration with this fingerprint.
func (c *Connection) IssuedFor(config string) bool { return c.s.IssuedFor == config }

// MayRevokeAt reports whether disconnect may send this connection's token to currentRevokeURL (the
// server's revoke URL now, nil when none): only an OAuth token, and only to the revoke URL in place
// when it was issued (exact string equality).
func (c *Connection) MayRevokeAt(currentRevokeURL *string) bool {
	return c.s.Kind == KindOAuth && c.s.RevokeURL != nil && currentRevokeURL != nil && *c.s.RevokeURL == *currentRevokeURL
}

// StatusFor is the status a person sees: a credential for an older server configuration counts as expired.
func (c *Connection) StatusFor(config string) Status {
	if c.IssuedFor(config) {
		return c.s.Status
	}
	return StatusExpired
}

// Refreshed stores new tokens from a refresh: a provider that does not rotate refresh tokens sends
// none back and the previous one stays. The connection becomes ready; UpdatedAt is now.
func (c *Connection) Refreshed(t SealedTokens, now time.Time) {
	c.s.Access = cloneSealed(t.Access)
	if !t.Refresh.IsEmpty() {
		c.s.Refresh = cloneSealed(t.Refresh)
	}
	c.s.ExpiresAt = cloneTime(t.ExpiresAt)
	c.s.Status = StatusReady
	c.s.UpdatedAt = now
}

// ErrReplaceRefreshPresence is returned by ReplaceSealed when it would add or drop a refresh token.
var ErrReplaceRefreshPresence = errors.New("a re-seal keeps the refresh token present or absent")

// ReplaceSealed swaps the sealed bytes for the same tokens sealed under another key (SECRETS_KEY
// rotation): not a new credential, so status, expiry and UpdatedAt stay. Adding or dropping a
// refresh token is not a re-seal and returns ErrReplaceRefreshPresence.
func (c *Connection) ReplaceSealed(access, refresh Sealed) error {
	if refresh.IsEmpty() != c.s.Refresh.IsEmpty() {
		return ErrReplaceRefreshPresence
	}
	c.s.Access = cloneSealed(access)
	c.s.Refresh = cloneSealed(refresh)
	return nil
}

// Expire turns a ready connection expired (the token ran out, the refresh was rejected, upstream
// rejected the PAT); UpdatedAt is now.
func (c *Connection) Expire(now time.Time) {
	c.s.Status = StatusExpired
	c.s.UpdatedAt = now
}

// IsNewerThan reports whether the connection changed after seen was read, e.g. refreshed by another
// process. Compared in milliseconds, like the reference's Date.
func (c *Connection) IsNewerThan(seen *Connection) bool {
	return c.s.UpdatedAt.UnixMilli() > seen.s.UpdatedAt.UnixMilli()
}
