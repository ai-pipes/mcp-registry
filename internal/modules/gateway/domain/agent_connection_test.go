package domain_test

import (
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

func TestAgentConnectionListedInOrder(t *testing.T) {
	want := []string{"ready", "needs_connection", "expired", "not_required"}
	got := domain.AllAgentConnections()
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("got %v", got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
}

func TestAgentConnectionNeedsNothingForSharedServers(t *testing.T) {
	for _, st := range []domain.PersonalStatus{domain.PersonalMissing, domain.PersonalReady, domain.PersonalExpired, domain.PersonalAbsent} {
		for _, k := range []domain.AuthKind{domain.AuthNone, domain.AuthSharedSecret} {
			if got := domain.AgentConnectionOf(k, st); got != domain.ConnectionNotRequired {
				t.Errorf("%s/%q: %q", k, st, got)
			}
		}
	}
}

func TestAgentConnectionOfPersonalServers(t *testing.T) {
	want := map[domain.PersonalStatus]domain.AgentConnection{
		domain.PersonalReady:   domain.ConnectionReady,
		domain.PersonalExpired: domain.ConnectionExpired,
		domain.PersonalMissing: domain.ConnectionNeedsConnection,
		domain.PersonalAbsent:  domain.ConnectionNeedsConnection,
	}
	for _, k := range []domain.AuthKind{domain.AuthOAuthUser, domain.AuthPATUser} {
		for st, w := range want {
			if got := domain.AgentConnectionOf(k, st); got != w {
				t.Errorf("%s/%q: got %q want %q", k, st, got, w)
			}
		}
	}
}

func TestAgentConnectionNeedsLink(t *testing.T) {
	cases := map[domain.AgentConnection]bool{
		domain.ConnectionNeedsConnection: true,
		domain.ConnectionExpired:         true,
		domain.ConnectionReady:           false,
		domain.ConnectionNotRequired:     false,
	}
	for c, w := range cases {
		if got := c.NeedsLink(); got != w {
			t.Errorf("%s: %v", c, got)
		}
	}
}

func TestConnectURLIsTheConnectionsPageOfThePublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:3000":     "http://localhost:3000/connections",
		"https://gw.example/base//": "https://gw.example/base/connections",
		"https://gw.example/":       "https://gw.example/connections",
	} {
		if got := domain.ConnectURL(in); got != want {
			t.Errorf("ConnectURL(%q) = %q, want %q", in, got, want)
		}
	}
}
