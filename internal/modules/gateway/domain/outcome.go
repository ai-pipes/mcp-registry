package domain

import "github.com/ai-pipes/mcp-registry/internal/kernel"

// CallOutcome is how a call_tool ended, as the call log stores it.
type CallOutcome string

// Every outcome of the call log.
const (
	OutcomeOK                    CallOutcome = "ok"
	OutcomeUpstreamError         CallOutcome = "upstream_error"
	OutcomeTimeout               CallOutcome = "timeout"
	OutcomeTransportError        CallOutcome = "transport_error"
	OutcomeDeniedNotFound        CallOutcome = "denied:not_found"
	OutcomeDeniedInvalidArgs     CallOutcome = "denied:invalid_arguments"
	OutcomeDeniedQuota           CallOutcome = "denied:quota"
	OutcomeDeniedNeedsConnection CallOutcome = "denied:needs_connection"
	OutcomeDeniedExpired         CallOutcome = "denied:expired"
)

var outcomes = []CallOutcome{
	OutcomeOK, OutcomeUpstreamError, OutcomeTimeout, OutcomeTransportError, OutcomeDeniedNotFound,
	OutcomeDeniedInvalidArgs, OutcomeDeniedQuota, OutcomeDeniedNeedsConnection, OutcomeDeniedExpired,
}

// AllOutcomes lists the outcomes in the order of the call log's check constraint (a copy).
func AllOutcomes() []CallOutcome { return append([]CallOutcome(nil), outcomes...) }

// ParseOutcome is the membership check: ok is false for anything that is not an outcome.
func ParseOutcome(s string) (CallOutcome, bool) {
	for _, o := range outcomes {
		if string(o) == s {
			return o, true
		}
	}
	return "", false
}

// CredentialFailure is why the person's upstream credential could not be had (the strings of
// connections' CredentialFailure).
type CredentialFailure string

// The credential failures.
const (
	CredentialNeedsConnection CredentialFailure = "needs_connection"
	CredentialExpired         CredentialFailure = "expired"
	CredentialUnavailable     CredentialFailure = "unavailable"
)

// OutcomeOfResult covers: an answer from upstream is ok unless it carries isError: true (absent and
// false are ok); the result itself is passed on unchanged.
func OutcomeOfResult(isError *bool) CallOutcome {
	if isError != nil && *isError {
		return OutcomeUpstreamError
	}
	return OutcomeOK
}

// OutcomeOfUpstreamFailure covers: a JSON-RPC error answer is upstream answering (upstream_error), a
// timeout has its own outcome, too large and transport failures are transport_error. (Unauthorized
// is handled by the call before it asks for an outcome.)
func OutcomeOfUpstreamFailure(kind kernel.UpstreamErrorKind) CallOutcome {
	switch kind {
	case kernel.UpstreamRejected:
		return OutcomeUpstreamError
	case kernel.UpstreamTimeout:
		return OutcomeTimeout
	default:
		return OutcomeTransportError
	}
}

// OutcomeOfCredentialFailure covers: the OAuth provider being down (unavailable) is the service being
// unreachable (transport_error), not a denial.
func OutcomeOfCredentialFailure(f CredentialFailure) CallOutcome {
	switch f {
	case CredentialUnavailable:
		return OutcomeTransportError
	case CredentialNeedsConnection:
		return OutcomeDeniedNeedsConnection
	default:
		return OutcomeDeniedExpired
	}
}
