package infra

import (
	"net/http"
	"testing"
)

func TestIssuersEqualIsWHATWGNormalisationAndNothingElse(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://idp.example", "https://idp.example/", true},
		{"https://idp.example/", "https://idp.example", true},
		{"HTTPS://IDP.Example/", "https://idp.example/", true},
		{"https://idp.example:443/", "https://idp.example", true},
		{"http://idp.example:80", "http://idp.example/", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/", true},
		{"https://idp.example/realms/x", "https://idp.example/realms/x", true},
		{"https://idp.example/#", "https://idp.example/", false},
		{"https://idp.example/realm#", "https://idp.example/realm", false},
		{"https://idp.example/a|b", "https://idp.example/a%7Cb", false},
		{"https://idp.example/a^b", "https://idp.example/a%5Eb", false},
		{"https://idp.example/a|b", "https://idp.example/a|b", true},
		{"https://idp.example/realm", "https://idp.example/realm/", false},
		{"https://idp.example/?x=1", "https://idp.example/", false},
		{"https://idp.example?x=1", "https://idp.example/?x=1", true},
		{"https://user@idp.example/", "https://idp.example/", false},
		{"https://idp.example:80/", "https://idp.example/", false},
		{"https://idp.example/a/../b", "https://idp.example/b", false},
		{"https://[::1]:443/", "https://[::1]/", true},
		{"https://idp.example:8443/", "https://idp.example/", false},
		{"http://idp.example:443/", "http://idp.example/", false},
		{"https://idp.example/", "http://idp.example/", false},
		{"https://idp.example/realms/x/", "https://idp.example/realms/x", false}, // no stripping on non-empty paths
		{"https://idp.example/Realms/x", "https://idp.example/realms/x", false},  // no path case folding
		{"https://idp.example", "https://other.example", false},
		{"", "https://idp.example", false},
		{"not a url", "not a url", false},
	}
	for _, c := range cases {
		if got := issuersEqual(c.a, c.b); got != c.want {
			t.Errorf("issuersEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// The reference ignores HTTP_PROXY, HTTPS_PROXY and NO_PROXY, and so do the other outbound clients:
// an install behind a proxy variable that signs in on 1.0.x must keep signing in.
func TestTheDefaultOIDCTransportUsesNoProxyFromTheEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	p := NewOIDCProvider(OIDCConfig{})
	w, ok := p.client.Transport.(*watchTransport)
	if !ok {
		t.Fatalf("transport %T", p.client.Transport)
	}
	tr, ok := w.next.(*http.Transport)
	if !ok {
		t.Fatalf("inner transport %T", w.next)
	}
	// Go reads the proxy variables once per process, so a Proxy function could answer nil here for
	// that reason alone: the assertion is that there is no Proxy function at all.
	if tr.Proxy != nil {
		t.Fatal("the transport has a Proxy function: it would honour HTTP_PROXY / HTTPS_PROXY")
	}
	if tr == http.DefaultTransport {
		t.Fatal("the shared default transport is used")
	}
}
