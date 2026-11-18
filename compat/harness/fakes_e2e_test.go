//go:build compat

package harness_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
	"github.com/ai-pipes/mcp-registry/compat/harness"
)

// TestContainerReachesFakesInTheTestProcess proves that the app container reaches a fake that
// runs in the test process: registering the server makes the app call the fake's tools/list.
func TestContainerReachesFakesInTheTestProcess(t *testing.T) {
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	s := harness.Shared(t)
	admin := s.Admin(t)
	slug := harness.Unique("reach")
	r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "none"))
	if r.Status != 201 {
		t.Fatalf("register: %d %s", r.Status, r.Body)
	}
	if n := up.ListRequests(); n < 1 {
		t.Fatalf("the fake saw %d tools/list requests: the container did not reach the test process", n)
	}
	var view struct {
		ToolCount     int     `json:"toolCount"`
		LastSyncError *string `json:"lastSyncError"`
	}
	admin.Do(t, "GET", "/api/servers/"+slug, nil).Decode(t, &view)
	if view.ToolCount != 1 || view.LastSyncError != nil {
		t.Fatalf("the fake's tool did not reach the catalog: %+v", view)
	}
}

// browser fetches absolute URLs of the fakes from the test process, never following redirects.
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

// pathOf turns the app's absolute redirect target into the path and query the client takes.
func pathOf(t *testing.T, s *harness.Stack, target string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(target, s.BaseURL)
	if !ok {
		t.Fatalf("redirect %q does not lead back to %s", target, s.BaseURL)
	}
	return rest
}

func TestFakeIdPSignsAPersonInThroughTheApp(t *testing.T) {
	idp := fakes.StartIdP(t)
	s := harness.Start(t, harness.Options{OIDC: idp})
	idp.NextLogin(fakes.Claims{
		Sub: harness.Unique("oidc"), Name: "Olga Admin", Email: "olga@example.test",
		Groups: []string{"mcp-admins"},
	})
	c := s.Anonymous()
	login := c.Do(t, "GET", "/api/auth/login", nil)
	if login.Status != 302 || !strings.HasPrefix(login.Header.Get("Location"), idp.Issuer+"/authorize?") {
		t.Fatalf("login: %d Location %q", login.Status, login.Header.Get("Location"))
	}
	back := locationAt(t, login.Header.Get("Location"))
	cb := c.Do(t, "GET", pathOf(t, s, back), nil)
	if cb.Status != 302 {
		t.Fatalf("callback: %d %s (app log: %s)", cb.Status, cb.Body, tail(s.Logs(t)))
	}
	var me struct {
		Name   string   `json:"name"`
		Role   string   `json:"role"`
		Groups []string `json:"groups"`
	}
	r := c.Do(t, "GET", "/api/auth/me", nil)
	if r.Status != 200 {
		t.Fatalf("me: %d %s; callback redirected to %q", r.Status, r.Body, cb.Header.Get("Location"))
	}
	r.Decode(t, &me)
	if me.Name != "Olga Admin" || me.Role != "admin" {
		t.Fatalf("me: %+v", me)
	}
	if idp.TokenRequests() != 1 {
		t.Fatalf("token requests: %d", idp.TokenRequests())
	}
}

func tail(s string) string {
	const keep = 1500
	if len(s) > keep {
		return s[len(s)-keep:]
	}
	return s
}

func TestFakeOAuthProviderConnectsAPersonThroughTheApp(t *testing.T) {
	provider := fakes.StartOAuthProvider(t)
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	s := harness.Shared(t)
	admin := s.Admin(t)
	slug := harness.Unique("oauth")
	r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "oauth_user", fakes.WithOAuth(provider, "read")))
	if r.Status != 201 {
		t.Fatalf("register: %d %s", r.Status, r.Body)
	}
	start := admin.Do(t, "POST", "/api/connections/"+slug+"/start", nil)
	var started struct {
		URL string `json:"url"`
	}
	if start.Status != 200 {
		t.Fatalf("start: %d %s", start.Status, start.Body)
	}
	start.Decode(t, &started)
	if !strings.HasPrefix(started.URL, provider.AuthorizeURL+"?") {
		t.Fatalf("start url %q", started.URL)
	}
	back := locationAt(t, started.URL)
	cb := admin.Do(t, "GET", pathOf(t, s, back), nil)
	if loc := cb.Header.Get("Location"); cb.Status != 302 || loc != "/connections?connected="+url.QueryEscape(slug) {
		t.Fatalf("callback: %d Location %q", cb.Status, loc)
	}
	var list []struct {
		Server string `json:"server"`
		Status string `json:"status"`
	}
	admin.Do(t, "GET", "/api/connections", nil).Decode(t, &list)
	status := ""
	for _, c := range list {
		if c.Server == slug {
			status = c.Status
		}
	}
	if status != "ready" {
		t.Fatalf("connection status %q in %+v", status, list)
	}
	reqs := provider.Requests()
	if len(reqs) != 1 || reqs[0].GrantType != "authorization_code" {
		t.Fatalf("provider requests: %+v", reqs)
	}
	if id, secret, ok := reqs[0].BasicAuth(); !ok || id != provider.ClientID || secret != provider.ClientSecret {
		t.Fatalf("the token request was not authenticated with HTTP Basic: %q", harness.Redact(reqs[0].Authorization))
	}
}

func TestFakeUpstreamServesPagesAndSeesTheCredential(t *testing.T) {
	up := fakes.StartUpstream(t, fakes.UpstreamOptions{
		Tools:         fakes.FixtureTools("gitea"),
		PageSize:      25,
		RequireHeader: [2]string{"Authorization", "Bearer shared-key"},
	})
	s := harness.Shared(t)
	admin := s.Admin(t)
	slug := harness.Unique("gitea")
	r := admin.Do(t, "POST", "/api/servers", fakes.ServerInput(up, slug, "shared_secret", fakes.WithSecret("shared-key")))
	if r.Status != 201 {
		t.Fatalf("register: %d %s", r.Status, r.Body)
	}
	var view struct {
		ToolCount int `json:"toolCount"`
	}
	admin.Do(t, "GET", "/api/servers/"+slug, nil).Decode(t, &view)
	if view.ToolCount != 61 || up.ListRequests() != 3 {
		t.Fatalf("tools %d (want 61) over %d pages (want 3)", view.ToolCount, up.ListRequests())
	}
	res, err := s.MCP(t, admin.Token(t, harness.Unique("tok"))).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "call_tool",
		Arguments: map[string]any{"tool": slug + ".actions_config_read", "arguments": map[string]any{"method": "list_repo_secrets"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("call_tool: %v %+v", err, res)
	}
	calls := up.Calls()
	if len(calls) != 1 || calls[0].Tool != "actions_config_read" || calls[0].Header.Get("Authorization") != "Bearer shared-key" {
		t.Fatalf("upstream calls: %d, first %+v", len(calls), calls)
	}
}
