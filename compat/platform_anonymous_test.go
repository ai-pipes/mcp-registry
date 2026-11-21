//go:build compat

package compat_test

// TestPlatformWithoutPeople: everything about the platform layer that is observable without a
// person, a session, a token or the operator CLI: what every response carries, how the origin
// check and the body limit refuse, what the access log keeps and leaves out, how the client address
// is taken, and the configuration read from files. It runs against an image that has only the
// platform layer, and it passes against the reference image. The order of the middleware is part
// of what it pins (a refusal by size comes before the origin check, which comes before routing;
// the security headers and the request id are on every answer, refusals included).

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/compat/harness"
)

func TestPlatformWithoutPeople(t *testing.T) {
	s := harness.Shared(t)
	anon := s.Anonymous()
	const html = "Accept: text/html"

	index := anon.Do(t, "GET", "/", nil, html)
	asset := regexp.MustCompile(`/assets/[^"']+\.js`).FindString(string(index.Body))
	if index.Status != 200 || asset == "" {
		t.Fatalf("GET /: %d, no /assets/*.js in %.200q", index.Status, index.Body)
	}

	t.Run("every response carries the security headers and a request id", func(t *testing.T) {
		for _, c := range []struct {
			name, path string
			hdr        []string
			status     int
		}{
			{"index", "/", []string{html}, 200},
			{"no such page, asking for html", "/no-such-page", []string{html}, 200},
			{"no such page, not asking for html", "/no-such-page", nil, 404},
			{"unknown api path", "/api/no-such", nil, 404},
			{"hashed asset", asset, nil, 200},
			{"missing asset", "/assets/no-such-file-abc123.js", nil, 404},
			{"client route", "/catalog", []string{html}, 200},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := anon.Do(t, "GET", c.path, nil, c.hdr...)
				if r.Status != c.status {
					t.Fatalf("GET %s: %d, want %d (%.100q)", c.path, r.Status, c.status, r.Body)
				}
				assertSecurityHeaders(t, "GET "+c.path, r, true)
			})
		}
		// Last, so the others have run: the reference image can answer 503 for a moment right after
		// it first reports healthy (see TestPlatformHealthz); an answer of either kind carries the
		// headers, which is what is asserted here. The status of /healthz is TestPlatformHealthz's.
		t.Run("healthz", func(t *testing.T) {
			r := anon.Do(t, "GET", "/healthz", nil)
			if r.Status != 200 && r.Status != 503 {
				t.Fatalf("GET /healthz: %d", r.Status)
			}
			assertSecurityHeaders(t, "GET /healthz", r, true)
		})
	})

	t.Run("a mutation is judged by its Origin before routing", func(t *testing.T) {
		const csrf = "csrf|cross-origin request rejected"
		for _, origin := range []string{"", "http://evil.example"} {
			hdr := []string{"Origin:"}
			if origin != "" {
				hdr = []string{"Origin: " + origin}
			}
			r := anon.Do(t, "POST", "/api/no-such", nil, hdr...)
			if r.Status != 403 || errorCode(t, r) != csrf {
				t.Errorf("Origin %q: %d %s", origin, r.Status, r.Body)
			}
			assertSecurityHeaders(t, "refused POST", r, true)
		}
		own := anon.Do(t, "POST", "/api/no-such", nil)
		if own.Status != 404 || errorCode(t, own) != "not_found|not found" {
			t.Errorf("the stack's own Origin: %d %s", own.Status, own.Body)
		}
	})

	t.Run("an oversized body is refused before the origin check and before routing", func(t *testing.T) {
		const want = "limit_exceeded|request body is too large"
		big := make([]byte, restBodyLimit+1)
		for _, c := range []struct {
			name string
			hdr  []string
		}{
			{"the stack's own origin, an unknown route", nil},
			{"a foreign origin", []string{"Origin: http://evil.example"}},
			{"no origin", []string{"Origin:"}},
		} {
			t.Run("declared length, "+c.name, func(t *testing.T) {
				r := anon.DoExpectingEarlyRefusal(t, "POST", "/api/no-such", big, c.hdr...)
				if r.Status != 413 || errorCode(t, r) != want {
					t.Fatalf("%d %.200s", r.Status, r.Body)
				}
				assertSecurityHeaders(t, "413", r, true)
			})
			t.Run("no declared length, "+c.name, func(t *testing.T) {
				r := anon.DoChunkedExpectingEarlyRefusal(t, "POST", "/api/no-such", big, c.hdr...)
				if r.Status != 413 || errorCode(t, r) != want {
					t.Fatalf("%d %.200s", r.Status, r.Body)
				}
				assertSecurityHeaders(t, "413", r, true)
			})
		}
	})

	t.Run("the access log keeps the path and nothing of the query or the credentials", func(t *testing.T) {
		marker := harness.Unique("sk_secret-token")
		probe := harness.Unique("probe")
		r := anon.Do(t, "GET", "/no-such?probe="+probe, nil, "Authorization: Bearer "+marker)
		id := r.Header.Get("X-Request-Id")
		if r.Status != 404 || !requestID.MatchString(id) {
			t.Fatalf("probe: %d, X-Request-Id %q", r.Status, id)
		}
		var recs []map[string]any
		harness.Eventually(t, "the access record of the probe (reqId "+id+")", func() bool {
			all, _ := logRecords(t, s.Logs(t))
			recs = nil
			for _, x := range all {
				if x["reqId"] == id {
					recs = append(recs, x)
				}
			}
			return len(recs) > 0
		})
		if len(recs) != 1 {
			t.Fatalf("%d records for one request: %v", len(recs), recs)
		}
		rec := recs[0]
		if rec["method"] != "GET" || rec["path"] != "/no-such" || rec["status"] != float64(404) {
			t.Fatalf("access log record: %v", rec)
		}
		logs := s.Logs(t)
		for what, value := range map[string]string{"the Authorization value": marker, "the query value": probe, "the query name": "probe="} {
			if strings.Contains(logs, value) {
				t.Fatalf("the app log contains %s", what)
			}
		}
	})

	t.Run("a forwarded address is ignored without TRUST_PROXY", func(t *testing.T) {
		const forwarded = "203.0.113.7"
		id := anon.Do(t, "GET", "/no-such-"+harness.Unique("p"), nil, "X-Forwarded-For: "+forwarded).Header.Get("X-Request-Id")
		if ip := accessRecordIP(t, s, id); ip == forwarded || ip == "" {
			t.Fatalf("the access record's ip is %q", ip)
		}
	})

	t.Run("a forwarded address is the client's with TRUST_PROXY", func(t *testing.T) {
		const forwarded = "203.0.113.7"
		proxied := harness.Start(t, harness.Options{Env: map[string]string{"TRUST_PROXY": "true"}})
		id := proxied.Anonymous().Do(t, "GET", "/no-such-"+harness.Unique("p"), nil, "X-Forwarded-For: "+forwarded).Header.Get("X-Request-Id")
		if ip := accessRecordIP(t, proxied, id); ip != forwarded {
			t.Fatalf("the access record's ip is %q, want %q", ip, forwarded)
		}
	})

	t.Run("secrets from files: the app is healthy and serves the shell", func(t *testing.T) {
		files := harness.Start(t, harness.Options{SecretsKeyFile: "/tmp/anonymous-secrets-key", PasswordFile: "/tmp/anonymous-pg-password"}) //nolint:gosec // G101: paths in the container, not credentials
		r := files.Anonymous().Do(t, "GET", "/", nil, html)
		if r.Status != http.StatusOK || !strings.Contains(string(r.Body), `<div id="root">`) {
			t.Fatalf("GET /: %d %.200s", r.Status, r.Body)
		}
		assertSecurityHeaders(t, "GET / with the secrets from files", r, true)
	})
}

// accessRecordIP waits for the access record of request id and returns its ip field.
func accessRecordIP(t *testing.T, s *harness.Stack, id string) string {
	t.Helper()
	if !requestID.MatchString(id) {
		t.Fatalf("X-Request-Id %q", id)
	}
	var ip string
	harness.Eventually(t, "the access record of reqId "+id, func() bool {
		all, _ := logRecords(t, s.Logs(t))
		for _, x := range all {
			if x["reqId"] == id {
				ip, _ = x["ip"].(string)
				return true
			}
		}
		return false
	})
	return ip
}
