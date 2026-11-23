package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

func TestOutcomesListedInOrder(t *testing.T) {
	want := []string{
		"ok", "upstream_error", "timeout", "transport_error", "denied:not_found",
		"denied:invalid_arguments", "denied:quota", "denied:needs_connection", "denied:expired",
	}
	got := domain.AllOutcomes()
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("outcome %d: got %q want %q", i, got[i], want[i])
		}
	}
	// the returned slice is a copy
	got[0] = "x"
	if domain.AllOutcomes()[0] != domain.OutcomeOK {
		t.Fatal("AllOutcomes exposes its backing array")
	}
}

func TestParseOutcome(t *testing.T) {
	if o, ok := domain.ParseOutcome("denied:quota"); !ok || o != domain.OutcomeDeniedQuota {
		t.Fatalf("got %q %v", o, ok)
	}
	for _, bad := range []string{"denied:other", "", "OK", " ok", "denied"} {
		if _, ok := domain.ParseOutcome(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestOutcomeOfResult(t *testing.T) {
	yes, no := true, false
	if domain.OutcomeOfResult(nil) != domain.OutcomeOK || domain.OutcomeOfResult(&no) != domain.OutcomeOK {
		t.Fatal("absent and false are ok")
	}
	if domain.OutcomeOfResult(&yes) != domain.OutcomeUpstreamError {
		t.Fatal("true is upstream_error")
	}
}

func TestOutcomeOfUpstreamFailure(t *testing.T) {
	cases := map[kernel.UpstreamErrorKind]domain.CallOutcome{
		kernel.UpstreamTimeout:   domain.OutcomeTimeout,
		kernel.UpstreamTooLarge:  domain.OutcomeTransportError,
		kernel.UpstreamTransport: domain.OutcomeTransportError,
		kernel.UpstreamRejected:  domain.OutcomeUpstreamError,
	}
	for in, want := range cases {
		if got := domain.OutcomeOfUpstreamFailure(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestOutcomeOfCredentialFailure(t *testing.T) {
	cases := map[domain.CredentialFailure]domain.CallOutcome{
		domain.CredentialNeedsConnection: domain.OutcomeDeniedNeedsConnection,
		domain.CredentialExpired:         domain.OutcomeDeniedExpired,
		domain.CredentialUnavailable:     domain.OutcomeTransportError,
	}
	for in, want := range cases {
		if got := domain.OutcomeOfCredentialFailure(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestOnUpstreamUnauthorized(t *testing.T) {
	cases := map[domain.AuthKind]domain.UnauthorizedAction{
		domain.AuthOAuthUser:    domain.RefreshAndRetry,
		domain.AuthPATUser:      domain.Expire,
		domain.AuthSharedSecret: domain.Reject,
		domain.AuthNone:         domain.Reject,
	}
	for in, want := range cases {
		if got := domain.OnUpstreamUnauthorized(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestAfterForcedRefresh(t *testing.T) {
	cases := map[domain.RefreshResult]domain.RefreshAction{
		domain.RefreshedSecretChanged: domain.RetryWithNewSecret,
		domain.RefreshedSecretSame:    domain.ReportUnavailable,
		domain.RefreshFailed:          domain.ReportRefreshFailure,
	}
	for in, want := range cases {
		if got := domain.AfterForcedRefresh(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
