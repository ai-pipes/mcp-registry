package domain

// UnauthorizedAction is what the gateway does when the upstream answers 401 to its credential.
type UnauthorizedAction string

// The actions after an upstream 401.
const (
	// RefreshAndRetry: a personal OAuth token is force-refreshed once and the call retried.
	RefreshAndRetry UnauthorizedAction = "refresh_and_retry"
	// Expire: a PAT cannot be renewed, the connection expires (no retry).
	Expire UnauthorizedAction = "expire"
	// Reject: the server's own key (or no auth) is the administrator's to fix.
	Reject UnauthorizedAction = "reject"
)

// OnUpstreamUnauthorized decides by the server's auth kind (the one read before the call).
func OnUpstreamUnauthorized(kind AuthKind) UnauthorizedAction {
	switch kind {
	case AuthOAuthUser:
		return RefreshAndRetry
	case AuthPATUser:
		return Expire
	default:
		return Reject
	}
}

// RefreshResult is the outcome of the forced refresh that follows an upstream 401 on an
// oauth_user server. The caller compares the two secrets itself; the rule never sees them.
type RefreshResult string

// The three possible results of the forced refresh.
const (
	// RefreshedSecretChanged: the forced resolve returned a secret other than the rejected one.
	RefreshedSecretChanged RefreshResult = "secret_changed"
	// RefreshedSecretSame: it returned the very secret upstream just rejected. Known causes: the
	// forced resolve joined a renewal of the same person and server that was not forced and whose
	// provider call got no answer (that renewal hands out the unexpired token), or the provider
	// answered the refresh with the same access token.
	RefreshedSecretSame RefreshResult = "secret_same"
	// RefreshFailed: the forced resolve returned no secret (a credential failure).
	RefreshFailed RefreshResult = "refresh_failed"
)

// RefreshAction is what to do after the forced refresh.
type RefreshAction string

// The actions after a forced refresh.
const (
	// RetryWithNewSecret: one more upstream call with the new secret; a second 401 then means
	// MarkExpired and the expired text (denied:expired).
	RetryWithNewSecret RefreshAction = "retry"
	// ReportUnavailable: no MarkExpired, no second upstream call; the agent gets
	// TextTransport and the outcome is transport_error (owner ruling of 2026-11-23).
	ReportUnavailable RefreshAction = "unavailable"
	// ReportRefreshFailure: answer from the credential failure the resolve returned
	// (OutcomeOfCredentialFailure and the matching text).
	ReportRefreshFailure RefreshAction = "refresh_failure"
)

// AfterForcedRefresh is the pure rule of the owner's decision: the same secret means the token
// was not renewed, so retrying would only repeat the 401 and expiring would punish the person
// for a provider outage.
func AfterForcedRefresh(r RefreshResult) RefreshAction {
	switch r {
	case RefreshedSecretChanged:
		return RetryWithNewSecret
	case RefreshedSecretSame:
		return ReportUnavailable
	default:
		return ReportRefreshFailure
	}
}
