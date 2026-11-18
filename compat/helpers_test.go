//go:build compat

package compat_test

// Helpers shared by several scenario files.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// browser fetches absolute URLs of the fakes (host.docker.internal) from the test process, as the
// person's browser would, and never follows redirects.
func browser() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && host == "host.docker.internal" {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return dialer.DialContext(ctx, network, addr)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// locationAt asks a fake's URL (the person's browser at the provider) and returns the 302 target.
func locationAt(t *testing.T, target string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := browser().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: status %d, want 302", target, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// cookieNamed returns the Set-Cookie of the response with that name, or nil.
func cookieNamed(r harness.Response, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// idpAttempt is a sign-in started at the app and carried through the provider up to the redirect
// back, with the callback not yet visited.
type idpAttempt struct {
	Client *harness.Client // the browser: its jar holds the one-time binding cookie
	Start  harness.Response
	// Binding is the "mcp_login=<value>" pair of the binding cookie ("" if none was set).
	Binding string
	// Back is the path and query of the callback URL the provider redirected to.
	Back string
	// Code is the authorization code the provider issued.
	Code string
}

// beginIDPLogin starts a sign-in (GET /api/auth/login) in a new browser, lets the provider issue a
// code for claims, and returns the attempt before the callback. returnTo "" sends no returnTo.
func beginIDPLogin(t *testing.T, s *harness.Stack, idp *fakes.FakeIdP, claims fakes.Claims, returnTo string) idpAttempt {
	t.Helper()
	idp.NextLogin(claims)
	c := s.Anonymous()
	path := "/api/auth/login"
	if returnTo != "" {
		path += "?returnTo=" + url.QueryEscape(returnTo)
	}
	start := c.Do(t, "GET", path, nil)
	loc := start.Header.Get("Location")
	if start.Status != 302 || !strings.HasPrefix(loc, idp.Issuer+"/authorize?") {
		t.Fatalf("GET %s: %d Location %q (body %.200q)", path, start.Status, loc, start.Body)
	}
	a := idpAttempt{Client: c, Start: start}
	if b := cookieNamed(start, "mcp_login"); b != nil {
		a.Binding = b.Name + "=" + b.Value
	}
	back := locationAt(t, loc)
	rest, ok := strings.CutPrefix(back, s.BaseURL)
	if !ok {
		t.Fatalf("the provider redirected to %q, not back to %s", back, s.BaseURL)
	}
	a.Back = rest
	u, err := url.Parse(back)
	if err != nil {
		t.Fatal(err)
	}
	a.Code = u.Query().Get("code")
	return a
}

// callback visits the callback URL with the attempt's own browser (its jar sends the binding).
func (a idpAttempt) callback(t *testing.T) harness.Response {
	t.Helper()
	return a.Client.Do(t, "GET", a.Back, nil)
}

// loginViaIDP signs a person in through the whole provider round trip and returns the browser
// with its session. The callback must lead to the returnTo "/" with a session cookie.
func loginViaIDP(t *testing.T, s *harness.Stack, idp *fakes.FakeIdP, claims fakes.Claims) *harness.Client {
	t.Helper()
	a := beginIDPLogin(t, s, idp, claims, "")
	cb := a.callback(t)
	if cb.Status != 302 || cb.Header.Get("Location") != "/" || cookieNamed(cb, "mcp_session") == nil {
		t.Fatalf("callback: %d Location %q session cookie %v (body %.200q)", cb.Status, cb.Header.Get("Location"), cookieNamed(cb, "mcp_session") != nil, cb.Body)
	}
	return a.Client
}

// meBody is the MeResponse contract.
type meBody struct {
	UserID string   `json:"userId"`
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Groups []string `json:"groups"`
}

// whoami returns GET /api/auth/me of a signed-in client; it fails the test when not signed in.
func whoami(t *testing.T, c *harness.Client) meBody {
	t.Helper()
	r := c.Do(t, "GET", "/api/auth/me", nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/auth/me: %d %s", r.Status, r.Body)
	}
	var m meBody
	r.JSON(t, "MeResponse", &m)
	return m
}

// auditEntry is the AuditEntry contract.
type auditEntry struct {
	ID      string         `json:"id"`
	At      string         `json:"at"`
	ActorID *string        `json:"actorId"`
	Action  string         `json:"action"`
	Target  *string        `json:"target"`
	Meta    map[string]any `json:"meta"`
}

// auditAll reads the whole audit log as admin (newest first), page by page.
func auditAll(t *testing.T, admin *harness.Client) []auditEntry {
	t.Helper()
	var all []auditEntry
	before := ""
	for {
		path := "/api/audit?limit=500"
		if before != "" {
			path += "&before=" + before
		}
		r := admin.Do(t, "GET", path, nil)
		if r.Status != 200 {
			t.Fatalf("GET %s: %d %s", path, r.Status, r.Body)
		}
		var page []auditEntry
		r.JSON(t, "AuditList", &page)
		all = append(all, page...)
		if len(page) < 500 {
			return all
		}
		before = page[len(page)-1].ID
	}
}

// auditWhere returns the entries with that action for which keep is true (nil keeps all).
func auditWhere(entries []auditEntry, action string, keep func(auditEntry) bool) []auditEntry {
	var out []auditEntry
	for _, e := range entries {
		if e.Action == action && (keep == nil || keep(e)) {
			out = append(out, e)
		}
	}
	return out
}

func ptrIs(p *string, want string) bool { return p != nil && *p == want }

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// countSQL runs a "select count(*)" style query.
func countSQL(t *testing.T, s *harness.Stack, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func execSQL(t *testing.T, s *harness.Stack, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustString(v any) string {
	s, _ := v.(string)
	return s
}

// firstLine is the first line of a CLI output.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// cookieDesc describes a cookie without its value.
func cookieDesc(c *http.Cookie) string {
	if c == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s (value %d chars) path %q httpOnly %v secure %v sameSite %v maxAge %d", c.Name, len(c.Value), c.Path, c.HttpOnly, c.Secure, c.SameSite, c.MaxAge)
}
