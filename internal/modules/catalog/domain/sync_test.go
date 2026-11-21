package domain_test

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

var allFailures = []domain.SyncFailure{
	domain.FailureNeedsConnection, domain.FailureNoSecret, domain.FailureUnauthorized, domain.FailureTimeout,
	domain.FailureTooLarge, domain.FailureTransport, domain.FailureCredentialsUnavailable, domain.FailureRejected,
	domain.FailureInternal,
}

func TestSyncFailureTextExactAndDistinct(t *testing.T) {
	want := map[domain.SyncFailure]string{
		domain.FailureNeedsConnection:        "An administrator needs to connect this server",
		domain.FailureCredentialsUnavailable: "The OAuth provider is unavailable",
		domain.FailureNoSecret:               "The server secret is not set",
		domain.FailureInternal:               "Internal sync error",
		domain.FailureUnauthorized:           "Upstream rejected the credentials (HTTP 401)",
		domain.FailureTimeout:                "Upstream did not respond in time",
		domain.FailureTooLarge:               "The upstream response exceeds the allowed size",
		domain.FailureTransport:              "Could not reach the upstream",
		domain.FailureRejected:               "Upstream answered tools/list with a JSON-RPC error",
	}
	seen := map[string]bool{}
	letters := regexp.MustCompile(`[A-Za-z]`)
	for _, f := range allFailures {
		got := domain.SyncFailureText(f)
		if got != want[f] || !letters.MatchString(got) || seen[got] {
			t.Errorf("%s: %q", f, got)
		}
		seen[got] = true
	}
	if len(want) != len(allFailures) {
		t.Fatal("table")
	}
	if domain.SyncFailureText("nonsense") != "Internal sync error" {
		t.Fatal("an unknown failure reads as internal")
	}
}

func TestSyncFailureStrings(t *testing.T) {
	want := map[domain.SyncFailure]string{
		domain.FailureNeedsConnection: "needs_connection", domain.FailureCredentialsUnavailable: "credentials_unavailable",
		domain.FailureNoSecret: "no_secret", domain.FailureInternal: "internal", domain.FailureUnauthorized: "unauthorized",
		domain.FailureTimeout: "timeout", domain.FailureTooLarge: "too_large", domain.FailureTransport: "transport",
		domain.FailureRejected: "rejected",
	}
	for f, s := range want {
		if string(f) != s {
			t.Errorf("%q != %q", f, s)
		}
	}
}

func TestSyncFailureOfUpstream(t *testing.T) {
	kinds := map[kernel.UpstreamErrorKind]domain.SyncFailure{
		kernel.UpstreamUnauthorized: domain.FailureUnauthorized, kernel.UpstreamTimeout: domain.FailureTimeout,
		kernel.UpstreamTooLarge: domain.FailureTooLarge, kernel.UpstreamTransport: domain.FailureTransport,
		kernel.UpstreamRejected: domain.FailureRejected,
	}
	for k, want := range kinds {
		if got := domain.SyncFailureOfUpstream(k); got != want {
			t.Errorf("%s: %s", k, got)
		}
	}
	if domain.SyncFailureOfUpstream("weird") != domain.FailureTransport {
		t.Fatal("an unknown kind is a transport failure")
	}
}

func TestSyncRecords(t *testing.T) {
	if got := domain.SyncSuccess(ptr("admin-1"), t0); !reflect.DeepEqual(got, domain.SyncRecord{StartedAt: t0, SyncedBy: ptr("admin-1")}) {
		t.Fatalf("%+v", got)
	}
	got := domain.SyncFailureRecord(domain.FailureNoSecret, t0)
	if got.Error == nil || *got.Error != "The server secret is not set" || got.SyncedBy != nil || !got.StartedAt.Equal(t0) {
		t.Fatalf("%+v", got)
	}
	if r := domain.SyncSuccess(nil, t0); r.SyncedBy != nil || r.Error != nil {
		t.Fatal("scheduled success")
	}
}

func TestMayLend(t *testing.T) {
	if !domain.MayLend(ptr("admin-1"), true) {
		t.Fatal("enabled admin lends")
	}
	if domain.MayLend(ptr("admin-1"), false) {
		t.Fatal("demoted or disabled admin does not")
	}
	if domain.MayLend(nil, true) {
		t.Fatal("nobody lends nothing")
	}
}

func TestFailureOfPersonalCredential(t *testing.T) {
	cases := map[domain.PersonalCredentialFailure]domain.SyncFailure{
		domain.CredentialNeedsConnection: domain.FailureNeedsConnection,
		domain.CredentialExpired:         domain.FailureNeedsConnection,
		domain.CredentialUnavailable:     domain.FailureCredentialsUnavailable,
	}
	for in, want := range cases {
		if got := domain.FailureOfCredential(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
