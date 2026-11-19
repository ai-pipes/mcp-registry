//go:build compat && e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/compat/e2e"
	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) { harness.Main(m) }

// ctl sends a request to the control API; token "" sends none.
func ctl(t *testing.T, base, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		enc, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(enc)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// withControl serves the control API of c while fn runs on the test goroutine.
func withControl(t *testing.T, c *e2e.Control, fn func(base string)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := c.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fn(base)
}

func TestControlRefusesEveryRequestWithoutTheRunToken(t *testing.T) {
	c := e2e.NewControl(t.Cleanup, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := c.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("the control API is not bound to the loopback interface: %s", base)
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/stacks"},
		{"POST", "/cli/create-user"},
		{"POST", "/cli/login-link"},
		{"POST", "/stack/first-start-link"},
		{"POST", "/idp/next-login"},
		{"POST", "/servers"},
		{"GET", "/upstream/calls?slug=x"},
		{"POST", "/sessions/expire"},
		{"GET", "/audit?stack=x"},
		{"GET", "/no-such-path"},
	} {
		var body any
		if r.method == "POST" {
			body = map[string]any{}
		}
		for name, token := range map[string]string{"no token": "", "wrong token": "not-" + c.Token()} {
			if status, resp := ctl(t, base, token, r.method, r.path, body); status != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d %s, want 401", r.method, r.path, name, status, resp)
			}
		}
	}
	// With the token the same requests reach the handlers (and are refused for their own reasons).
	if status, _ := ctl(t, base, c.Token(), "GET", "/stacks", nil); status != http.StatusOK {
		t.Errorf("GET /stacks with the token: %d", status)
	}
	if status, _ := ctl(t, base, c.Token(), "GET", "/no-such-path", nil); status != http.StatusNotFound {
		t.Errorf("an unknown path with the token: %d, want 404", status)
	}
	if c.Token() == e2e.NewControl(t.Cleanup, nil, nil).Token() {
		t.Error("two controls share a token")
	}
	// A bearer scheme only: the bare token is not accepted.
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/stacks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", c.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the bare token: %d", resp.StatusCode)
	}
}

func TestControlRefusesBadParametersBeforeTouchingAnything(t *testing.T) {
	c := e2e.NewControl(t.Cleanup, nil, nil) // no stacks, no IdP: every refusal below happens before any is needed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := c.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"an unknown field", "POST", "/cli/create-user", map[string]any{"stack": "x", "sub": "a", "name": "b", "extra": 1}, 400},
		{"an option as a sub", "POST", "/cli/create-user", map[string]any{"stack": "x", "sub": "--admin", "name": "b"}, 400},
		{"a name with a newline", "POST", "/cli/create-user", map[string]any{"stack": "x", "sub": "a", "name": "b\nc"}, 400},
		{"an unknown stack", "POST", "/cli/create-user", map[string]any{"stack": "x", "sub": "a", "name": "b"}, 404},
		{"a sub that is not a word", "POST", "/cli/login-link", map[string]any{"stack": "x", "sub": "a; rm -rf /"}, 400},
		{"login-link of an unknown stack", "POST", "/cli/login-link", map[string]any{"stack": "x", "sub": "a"}, 404},
		{"first-start-link of an unknown stack", "POST", "/stack/first-start-link", map[string]any{"stack": "x"}, 404},
		{"no identity provider", "POST", "/idp/next-login", map[string]any{"sub": "a", "name": "b"}, 404},
		{"a server of an unknown stack", "POST", "/servers", map[string]any{"stack": "x", "kind": "none"}, 404},
		{"an unknown session stack", "POST", "/sessions/expire", map[string]any{"stack": "x", "sub": "a"}, 404},
		{"a body that is not JSON", "POST", "/sessions/expire", "not json", 400},
		{"an actor that is not a sub", "GET", "/audit?stack=x&actor=--admin", nil, 400},
		{"an action that is not a name", "GET", "/audit?stack=x&action=DROP%20TABLE", nil, 400},
		{"audit of an unknown stack", "GET", "/audit?stack=x", nil, 404},
		{"calls of an unknown server", "GET", "/upstream/calls?slug=nope", nil, 404},
	} {
		var status int
		var resp []byte
		if s, ok := tc.body.(string); ok { // sent as it is
			req, err := http.NewRequestWithContext(ctx, tc.method, base+tc.path, strings.NewReader(s))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+c.Token())
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
			status = r.StatusCode
		} else {
			status, resp = ctl(t, base, c.Token(), tc.method, tc.path, tc.body)
		}
		if status != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, status, resp, tc.want)
		}
	}
}

func TestControlRefusesBadServerParameters(t *testing.T) {
	// Parameter checks on /servers come after the stack lookup, so they need a stack entry; the
	// entry is never booted (a nil harness stack) because every case is refused first.
	c := e2e.NewControl(t.Cleanup, []*e2e.Stack{{Name: "s"}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, err := c.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{
		"an unknown kind":        {"stack": "s", "kind": "magic"},
		"oauth without provider": {"stack": "s", "kind": "oauth_user"},
		"a bad slug":             {"stack": "s", "kind": "none", "slug": "Bad Slug"},
		"a bad tool name":        {"stack": "s", "kind": "none", "tools": []string{"ok", "no way"}},
		"too many tools":         {"stack": "s", "kind": "none", "tools": make([]string, 21)},
	} {
		if status, resp := ctl(t, base, c.Token(), "POST", "/servers", body); status != 400 {
			t.Errorf("%s: %d %s, want 400", name, status, resp)
		}
	}
}

func TestControlOperationsAgainstARealStack(t *testing.T) {
	idp := fakes.StartIdP(t)
	provider := fakes.StartOAuthProvider(t)
	plain := harness.Start(t, harness.Options{})
	oidc := harness.Start(t, harness.Options{OIDC: idp})
	c := e2e.NewControl(t.Cleanup, []*e2e.Stack{
		{Name: "plain", Harness: plain, Provider: provider},
		{Name: "oidc", Harness: oidc, OIDC: true},
	}, idp)

	withControl(t, c, func(base string) {
		call := func(method, path string, body any, want int, into any) {
			t.Helper()
			status, resp := ctl(t, base, c.Token(), method, path, body)
			if status != want {
				t.Errorf("%s %s: %d %s, want %d", method, path, status, resp, want)
				return
			}
			if into != nil {
				if err := json.Unmarshal(resp, into); err != nil {
					t.Errorf("%s %s: %v in %s", method, path, err, resp)
				}
			}
		}

		// GET /stacks tells which is which.
		var stacks struct {
			Stacks map[string]e2e.StackInfo `json:"stacks"`
		}
		call("GET", "/stacks", nil, 200, &stacks)
		if len(stacks.Stacks) != 2 || stacks.Stacks["plain"].BaseURL != plain.BaseURL || stacks.Stacks["plain"].OIDC ||
			stacks.Stacks["plain"].ProviderURL != provider.AuthorizeURL || stacks.Stacks["oidc"].BaseURL != oidc.BaseURL || !stacks.Stacks["oidc"].OIDC {
			t.Errorf("stacks: %+v", stacks)
		}

		// first-start-link: the link in the log signs local-admin in.
		var first struct{ Link string }
		call("POST", "/stack/first-start-link", map[string]string{"stack": "plain"}, 200, &first)
		if !strings.HasPrefix(first.Link, plain.BaseURL+"/login#link=") {
			t.Errorf("first-start link %q", harness.Redact(first.Link))
		}
		call("POST", "/stack/first-start-link", map[string]string{"stack": "oidc"}, 404, nil) // OIDC: no link is logged

		// create-user then login-link: a link that works once, for that person.
		call("POST", "/cli/create-user", map[string]any{"stack": "plain", "sub": "ctl-member", "name": "Ctl Member", "admin": false}, 200, nil)
		call("POST", "/cli/create-user", map[string]any{"stack": "plain", "sub": "ctl-member", "name": "Ctl Member", "admin": false}, 200, nil) // idempotent
		var link struct{ Link string }
		call("POST", "/cli/login-link", map[string]string{"stack": "plain", "sub": "ctl-member"}, 200, &link)
		_, code, _ := strings.Cut(link.Link, "#link=")
		c1 := plain.Anonymous()
		if r := c1.Do(t, "POST", "/api/auth/link", map[string]string{"code": code}); r.Status != 204 {
			t.Errorf("sign-in with the link: %d %s", r.Status, r.Body)
		}
		if r := c1.Do(t, "GET", "/api/auth/me", nil); !strings.Contains(string(r.Body), `"name":"Ctl Member"`) || !strings.Contains(string(r.Body), `"role":"member"`) {
			t.Errorf("me: %d %s", r.Status, r.Body)
		}
		call("POST", "/cli/login-link", map[string]string{"stack": "plain", "sub": "nobody-here"}, 502, nil) // an unknown person: the CLI fails

		// create-user --admin makes an admin.
		call("POST", "/cli/create-user", map[string]any{"stack": "plain", "sub": "ctl-admin", "name": "Ctl Admin", "admin": true}, 200, nil)
		call("POST", "/cli/login-link", map[string]string{"stack": "plain", "sub": "ctl-admin"}, 200, &link)
		_, code, _ = strings.Cut(link.Link, "#link=")
		c2 := plain.Anonymous()
		c2.Do(t, "POST", "/api/auth/link", map[string]string{"code": code})
		if r := c2.Do(t, "GET", "/api/auth/me", nil); !strings.Contains(string(r.Body), `"role":"admin"`) {
			t.Errorf("admin me: %d %s", r.Status, r.Body)
		}

		// sessions/expire: that person's session ends, the other one's does not.
		call("POST", "/sessions/expire", map[string]string{"stack": "plain", "sub": "ctl-member"}, 200, nil)
		if r := c1.Do(t, "GET", "/api/auth/me", nil); r.Status != 401 {
			t.Errorf("an expired session: %d %s", r.Status, r.Body)
		}
		if r := c2.Do(t, "GET", "/api/auth/me", nil); r.Status != 200 {
			t.Errorf("another person's session: %d %s", r.Status, r.Body)
		}

		// servers: one of every kind, each as the gateway sees it, and the calls its upstream got.
		admin := plain.Admin(t)
		type view struct {
			Auth struct {
				Kind  string `json:"kind"`
				OAuth struct {
					AuthorizeURL string `json:"authorizeUrl"`
				} `json:"oauth"`
			} `json:"auth"`
			SecretSet     bool    `json:"secretSet"`
			ToolCount     int     `json:"toolCount"`
			LastSyncError *string `json:"lastSyncError"`
			Name          string  `json:"name"`
		}
		for _, k := range []struct {
			kind      string
			tools     []string
			wantTools int
			secret    bool
		}{
			{"none", []string{"alpha", "beta", "gamma"}, 3, false},
			{"shared_secret", nil, 2, true}, // the upstream demands the secret: the sync only passes when it was sent
			{"pat_user", nil, 0, false},     // personal credentials: the control admin has none, so nothing is synced
			{"oauth_user", nil, 0, false},
		} {
			slug := "ctl-" + strings.ReplaceAll(k.kind, "_", "-")
			var reg e2e.RegisteredServer
			call("POST", "/servers", map[string]any{"stack": "plain", "kind": k.kind, "slug": slug, "tools": k.tools}, 200, &reg)
			if reg.Slug != slug || reg.Name != fakes.DisplayName(slug) || !strings.HasPrefix(reg.URL, "http://host.docker.internal:") || len(reg.Tools) == 0 {
				t.Errorf("%s: registered %+v", k.kind, reg)
			}
			var v view
			admin.Do(t, "GET", "/api/servers/"+slug, nil).Decode(t, &v)
			if v.Auth.Kind != k.kind || v.ToolCount != k.wantTools || v.SecretSet != (k.secret || k.kind == "oauth_user") || v.Name != fakes.DisplayName(slug) {
				t.Errorf("%s: view %+v", k.kind, v)
			}
			if k.kind == "oauth_user" && v.Auth.OAuth.AuthorizeURL != provider.AuthorizeURL {
				t.Errorf("oauth_user: authorize URL %q", v.Auth.OAuth.AuthorizeURL)
			}
		}
		call("POST", "/servers", map[string]any{"stack": "plain", "kind": "none", "slug": "ctl-none"}, 502, nil) // the slug is taken
		call("POST", "/servers", map[string]any{"stack": "oidc", "kind": "oauth_user"}, 400, nil)                // that stack has no provider

		// audit: the registrations above are in the trail, by the control admin, whatever the order.
		var trail struct {
			Rows []e2e.AuditRow `json:"rows"`
		}
		call("GET", "/audit?stack=plain&actor=e2e-control&action=server.create", nil, 200, &trail)
		targets := map[string]bool{}
		for _, r := range trail.Rows {
			if r.Action != "server.create" || r.ActorSub != "e2e-control" {
				t.Errorf("audit row outside the filter: %+v", r)
			}
			targets[r.Target] = true
		}
		for _, slug := range []string{"ctl-none", "ctl-shared-secret", "ctl-pat-user", "ctl-oauth-user"} {
			if !targets[slug] {
				t.Errorf("no server.create row for %s in %+v", slug, trail.Rows)
			}
		}
		call("GET", "/audit?stack=plain&actor=ctl-member&action=server.create", nil, 200, &trail)
		if len(trail.Rows) != 0 {
			t.Errorf("another person's filter returned %+v", trail.Rows)
		}
		call("GET", "/audit?stack=plain", nil, 200, &trail)
		if len(trail.Rows) < 4 {
			t.Errorf("unfiltered audit: %d rows", len(trail.Rows))
		}

		type calls struct {
			Calls []e2e.UpstreamCall `json:"calls"`
		}
		var seen calls
		call("GET", "/upstream/calls?slug=ctl-none", nil, 200, &seen)
		if len(seen.Calls) != 0 {
			t.Errorf("calls before any call: %+v", seen)
		}
		res, err := plain.MCP(t, admin.Token(t, harness.Unique("tok"))).CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "call_tool",
			Arguments: map[string]any{"tool": "ctl-none.alpha", "arguments": map[string]any{"text": "hi"}},
		})
		if err != nil || res.IsError {
			t.Fatalf("call_tool: %v %+v", err, res)
		}
		call("GET", "/upstream/calls?slug=ctl-none", nil, 200, &seen)
		if len(seen.Calls) != 1 || seen.Calls[0].Tool != "alpha" || string(seen.Calls[0].Arguments) != `{"text":"hi"}` {
			t.Errorf("calls after one call: %+v", seen)
		}
		for _, h := range seen.Calls[0].HeaderNames {
			if strings.EqualFold(h, "authorization") {
				t.Errorf("an Authorization header reached a server of kind none: %v", seen.Calls[0].HeaderNames)
			}
		}

		// next-login: the claims reach the app through a real sign-in chain.
		call("POST", "/idp/next-login", map[string]any{"sub": "ctl-oidc", "name": "Ctl Oidc", "email": "o@example.test", "groups": []string{"mcp-admins"}}, 200, nil)
		oc := oidc.Anonymous()
		login := oc.Do(t, "GET", "/api/auth/login", nil)
		back := ctlFollow(t, login.Header.Get("Location"))
		if cb := oc.Do(t, "GET", strings.TrimPrefix(back, oidc.BaseURL), nil); cb.Status != 302 {
			t.Fatalf("oidc callback: %d %s", cb.Status, cb.Body)
		}
		var me struct {
			Name, Role string
			Groups     []string
		}
		oc.Do(t, "GET", "/api/auth/me", nil).Decode(t, &me)
		if me.Name != "Ctl Oidc" || me.Role != "admin" || len(me.Groups) != 1 || me.Groups[0] != "mcp-admins" {
			t.Errorf("signed in as %+v", me)
		}
		call("POST", "/idp/next-login", map[string]any{"sub": "", "name": ""}, 400, nil)
	})
}

// ctlFollow asks a fake's absolute URL as a browser would (host.docker.internal is the loopback
// address here) and returns the redirect target.
func ctlFollow(t *testing.T, target string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(target, "host.docker.internal", "127.0.0.1", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s: %d", target, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}
