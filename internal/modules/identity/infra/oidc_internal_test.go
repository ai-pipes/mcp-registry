package infra

import "testing"

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
