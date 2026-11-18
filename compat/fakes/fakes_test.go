//go:build compat

package fakes_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-pipes/mcp-registry/compat/fakes"
)

// localClient reaches host.docker.internal URLs from the test process through the loopback
// interface, like the harness client does. It never follows redirects.
func localClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, port, err := net.SplitHostPort(addr); err == nil && host == "host.docker.internal" {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func send(t *testing.T, method, target, body string, hdr ...string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	resp, err := localClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(data)
}

const rpcList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

var mcpHeaders = []string{"Content-Type: application/json", "Accept: application/json, text/event-stream"}

func connect(t *testing.T, u *fakes.Upstream, hdr ...string) *mcp.ClientSession {
	t.Helper()
	hc := localClient()
	if len(hdr) > 0 {
		hc.Transport = &headerTransport{base: hc.Transport, hdr: hdr}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fakes-test", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             u.URL,
		HTTPClient:           hc,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type headerTransport struct {
	base http.RoundTripper
	hdr  []string
}

func (h *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for _, x := range h.hdr {
		k, v, _ := strings.Cut(x, ": ")
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

func names(tools []fakes.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name)
	}
	return out
}

func TestFixtureTools(t *testing.T) {
	if n := len(fakes.FixtureTools("gitea")); n != 61 {
		t.Fatalf("gitea tools: %d", n)
	}
	if n := len(fakes.FixtureTools("youtrack")); n != 23 {
		t.Fatalf("youtrack tools: %d", n)
	}
	for _, tl := range fakes.FixtureTools("gitea") {
		if tl.Name == "" || tl.InputSchema == nil {
			t.Fatalf("incomplete tool %+v", tl)
		}
	}
}

func TestUpstreamServesEveryPageOfToolsList(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a", "b", "c", "d", "e"), PageSize: 2})
	if !strings.HasPrefix(u.URL, "http://host.docker.internal:") || !strings.HasSuffix(u.URL, "/mcp") {
		t.Fatalf("url %q", u.URL)
	}
	var got []string
	for tool, err := range connect(t, u).Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tool.Name)
	}
	if strings.Join(got, ",") != "a,b,c,d,e" {
		t.Fatalf("tools: %v", got)
	}
	if n := u.ListRequests(); n != 3 {
		t.Fatalf("tools/list requests: %d, want 3 pages", n)
	}
}

func TestUpstreamSetToolsAndReset(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
	u.SetTools(fakes.Tools("x", "y"))
	res, err := connect(t, u).ListTools(context.Background(), nil)
	if err != nil || len(res.Tools) != 2 || res.Tools[0].Name != "x" {
		t.Fatalf("tools %v err %v", res, err)
	}
	u.Reset()
	if u.ListRequests() != 0 || len(u.Calls()) != 0 {
		t.Fatal("Reset must clear the recorded requests")
	}
}

func TestUpstreamFailWithAnswersEveryRequest(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
	u.FailWith(401)
	status, hdr, _ := send(t, "POST", u.URL, rpcList, mcpHeaders...)
	if status != 401 || !strings.HasPrefix(hdr.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("401 mode: %d %v", status, hdr)
	}
	u.FailWith(500)
	if status, hdr, _ = send(t, "POST", u.URL, rpcList, mcpHeaders...); status != 500 || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("500 mode: %d %v", status, hdr)
	}
	u.Reset()
	if status, _, _ = send(t, "POST", u.URL, rpcList, mcpHeaders...); status != 200 {
		t.Fatalf("after Reset: %d", status)
	}
}

func TestUpstreamRequireHeader(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{
		Tools:         fakes.Tools("a"),
		RequireHeader: [2]string{"Authorization", "Bearer s3cret"},
	})
	if status, hdr, _ := send(t, "POST", u.URL, rpcList, mcpHeaders...); status != 401 || hdr.Get("WWW-Authenticate") == "" {
		t.Fatalf("no header: %d %v", status, hdr)
	}
	if status, _, _ := send(t, "POST", u.URL, rpcList, append(mcpHeaders, "Authorization: Bearer wrong")...); status != 401 {
		t.Fatalf("wrong value: %d", status)
	}
	res, err := connect(t, u, "Authorization: Bearer s3cret").ListTools(context.Background(), nil)
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("with header: %v %v", res, err)
	}
}

func TestUpstreamRecordsCallsWithHeaders(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	res, err := connect(t, u, "X-Probe: yes").CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %v", res, err)
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "hello") {
		t.Fatalf("the default answer echoes the arguments: %+v", res.Content)
	}
	calls := u.Calls()
	if len(calls) != 1 || calls[0].Tool != "echo" || calls[0].Header.Get("X-Probe") != "yes" {
		t.Fatalf("calls: %+v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil || args["text"] != "hello" {
		t.Fatalf("arguments %s: %v", calls[0].Arguments, err)
	}
}

func TestUpstreamRejectCalls(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	u.RejectCalls(-32602, "bad things")
	_, err := connect(t, u).CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"})
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "bad things" {
		t.Fatalf("want a JSON-RPC error -32602, got %v", err)
	}
}

func TestUpstreamRespondBytesHasExactSize(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("echo")})
	u.RespondBytes(100_000)
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	status, hdr, got := send(t, "POST", u.URL, body, mcpHeaders...)
	if status != 200 || len(got) != 100_000 || hdr.Get("Content-Length") != "100000" {
		t.Fatalf("status %d, body %d bytes, Content-Length %q", status, len(got), hdr.Get("Content-Length"))
	}
	var rpc struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &rpc); err != nil || rpc.ID != 7 || len(rpc.Result.Content) != 1 {
		t.Fatalf("not a JSON-RPC result: %v", err)
	}
	// tools/list is not affected.
	if status, _, got = send(t, "POST", u.URL, rpcList, mcpHeaders...); status != 200 || len(got) >= 100_000 {
		t.Fatalf("tools/list: %d, %d bytes", status, len(got))
	}
}

func TestUpstreamRedirectTo(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
	u.RedirectTo("http://elsewhere.example/mcp")
	status, hdr, _ := send(t, "POST", u.URL, rpcList, mcpHeaders...)
	if status != 307 || hdr.Get("Location") != "http://elsewhere.example/mcp" {
		t.Fatalf("redirect: %d %v", status, hdr)
	}
}

func TestUpstreamDelayHonoursTheRequestContext(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{Tools: fakes.Tools("a")})
	u.Delay(300 * time.Millisecond)
	start := time.Now()
	if status, _, _ := send(t, "POST", u.URL, rpcList, mcpHeaders...); status != 200 {
		t.Fatalf("status %d", status)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("answered after %s, want at least the delay", d)
	}
	u.Delay(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", u.URL, strings.NewReader(rpcList))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range mcpHeaders {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	start = time.Now()
	if resp, err := localClient().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the delayed request must not be answered")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the caller's deadline was not honoured")
	}
}

func TestServerInputShapes(t *testing.T) {
	u := fakes.StartUpstream(t, fakes.UpstreamOptions{})
	none := fakes.ServerInput(u, "s1", "none")
	if none["slug"] != "s1" || none["url"] != u.URL || none["name"] == "" {
		t.Fatalf("none: %v", none)
	}
	if a, _ := none["auth"].(map[string]any); a["kind"] != "none" {
		t.Fatalf("auth: %v", none["auth"])
	}
	shared := fakes.ServerInput(u, "s2", "shared_secret", fakes.WithSecret("abc"), fakes.WithGroups("devs"))
	auth, _ := shared["auth"].(map[string]any)
	if shared["secret"] != "abc" || auth["header"] != "Authorization" || auth["template"] != "Bearer {secret}" {
		t.Fatalf("shared_secret: %v", shared)
	}
	if acc, _ := shared["access"].(map[string]any); acc["kind"] != "groups" {
		t.Fatalf("access: %v", shared["access"])
	}
	p := fakes.StartOAuthProvider(t)
	oauth := fakes.ServerInput(u, "s3", "oauth_user", fakes.WithOAuth(p, "read"))
	cfg, _ := oauth["auth"].(map[string]any)["oauth"].(map[string]any)
	if cfg["authorizeUrl"] != p.AuthorizeURL || cfg["tokenUrl"] != p.TokenURL || cfg["clientId"] != p.ClientID || oauth["secret"] != p.ClientSecret {
		t.Fatalf("oauth_user: %v", oauth)
	}
}

// --- identity provider ---

func authorizeURL(idp *fakes.FakeIdP, verifier, nonce string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {idp.ClientID},
		"redirect_uri":          {"http://localhost:3000/api/auth/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid profile email groups"},
		"state":                 {"st-1"},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return idp.Issuer + "/authorize?" + q.Encode()
}

func login(t *testing.T, idp *fakes.FakeIdP, verifier, nonce string) (idToken, accessToken string) {
	t.Helper()
	status, hdr, _ := send(t, "GET", authorizeURL(idp, verifier, nonce), "")
	loc, err := url.Parse(hdr.Get("Location"))
	if status != 302 || err != nil {
		t.Fatalf("authorize: %d %v %v", status, hdr, err)
	}
	if loc.Query().Get("state") != "st-1" || loc.Query().Get("iss") != idp.Issuer || loc.Host != "localhost:3000" {
		t.Fatalf("authorize redirect: %s", loc)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri":  {"http://localhost:3000/api/auth/callback"},
		"code_verifier": {verifier}, "client_id": {idp.ClientID}, "client_secret": {idp.ClientSecret},
	}
	status, _, body := send(t, "POST", idp.Issuer+"/token", form.Encode(), "Content-Type: application/x-www-form-urlencoded")
	if status != 200 {
		t.Fatalf("token: %d %s", status, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatal(err)
	}
	return tok.IDToken, tok.AccessToken
}

func TestIdPPassesDiscoveryAndTokenVerification(t *testing.T) {
	idp := fakes.StartIdP(t)
	idp.NextLogin(fakes.Claims{Sub: "u-1", Name: "Una", Email: "una@example.test", Groups: []string{"mcp-admins"}})
	ctx := oidc.ClientContext(context.Background(), localClient())
	provider, err := oidc.NewProvider(ctx, idp.Issuer)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	idToken, _ := login(t, idp, "verifier-verifier-verifier-verifier-1234", "n-1")
	tok, err := provider.Verifier(&oidc.Config{ClientID: idp.ClientID}).Verify(ctx, idToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var claims struct {
		Sub    string   `json:"sub"`
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		Nonce  string   `json:"nonce"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "u-1" || claims.Name != "Una" || claims.Nonce != "n-1" || len(claims.Groups) != 1 || claims.Groups[0] != "mcp-admins" {
		t.Fatalf("claims: %+v", claims)
	}
	if idp.TokenRequests() != 1 {
		t.Fatalf("token requests: %d", idp.TokenRequests())
	}
}

func TestIdPRejectsAWrongVerifierOrSecret(t *testing.T) {
	idp := fakes.StartIdP(t)
	_, hdr, _ := send(t, "GET", authorizeURL(idp, "right-right-right-right-right-right-1", "n"), "")
	loc, _ := url.Parse(hdr.Get("Location"))
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"code_verifier": {"wrong"}, "client_id": {idp.ClientID}, "client_secret": {idp.ClientSecret},
	}
	status, _, body := send(t, "POST", idp.Issuer+"/token", form.Encode(), "Content-Type: application/x-www-form-urlencoded")
	if status != 400 || !strings.Contains(body, "invalid_grant") {
		t.Fatalf("wrong verifier: %d %s", status, body)
	}
}

func TestIdPGroupsAsObjectInUserInfoOnly(t *testing.T) {
	idp := fakes.StartIdP(t)
	idp.NextLogin(fakes.Claims{
		Sub: "u-2", Name: "Dee", Groups: []string{"mcp-users", "x"},
		GroupsShape: fakes.GroupsObject, Where: fakes.InUserInfo,
	})
	idToken, access := login(t, idp, "verifier-verifier-verifier-verifier-1234", "n-2")
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(idToken, ".")[1])
	if bytes.Contains(payload, []byte("mcp-users")) {
		t.Fatalf("groups must not be in the ID token: %s", payload)
	}
	status, _, body := send(t, "GET", idp.Issuer+"/userinfo", "", "Authorization: Bearer "+access)
	var info struct {
		Groups map[string]any `json:"groups"`
	}
	if err := json.Unmarshal([]byte(body), &info); status != 200 || err != nil || len(info.Groups) != 2 {
		t.Fatalf("userinfo: %d %s (%v)", status, body, err)
	}
	if status, _, _ = send(t, "GET", idp.Issuer+"/userinfo", "", "Authorization: Bearer nope"); status != 401 {
		t.Fatalf("userinfo with a bad token: %d", status)
	}
}

func TestIdPDownAnswers503(t *testing.T) {
	idp := fakes.StartIdP(t)
	idp.Down(true)
	if status, _, _ := send(t, "GET", idp.Issuer+"/.well-known/openid-configuration", ""); status != 503 {
		t.Fatalf("discovery while down: %d", status)
	}
	if status, _, _ := send(t, "POST", idp.Issuer+"/token", "grant_type=authorization_code"); status != 503 {
		t.Fatalf("token while down: %d", status)
	}
	if idp.TokenRequests() != 1 {
		t.Fatalf("token requests: %d", idp.TokenRequests())
	}
	idp.Down(false)
	if status, _, _ := send(t, "GET", idp.Issuer+"/.well-known/openid-configuration", ""); status != 200 {
		t.Fatalf("discovery after recovery: %d", status)
	}
}

// --- OAuth provider ---

func tokenCall(t *testing.T, p *fakes.OAuthProvider, form url.Values) (int, string) {
	t.Helper()
	basic := base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(p.ClientID) + ":" + url.QueryEscape(p.ClientSecret)))
	status, _, body := send(t, "POST", p.TokenURL, form.Encode(),
		"Content-Type: application/x-www-form-urlencoded", "Authorization: Basic "+basic)
	return status, body
}

func TestOAuthProviderAnswersTheScriptInOrder(t *testing.T) {
	p := fakes.StartOAuthProvider(t)
	p.Script(
		fakes.Ok{AccessToken: "acc-1", RefreshToken: "ref-1", ExpiresIn: 120},
		fakes.Error{Status: 400, Code: "invalid_grant"},
	)
	status, body := tokenCall(t, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"r0"}})
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(body), &tok); status != 200 || err != nil || tok.AccessToken != "acc-1" || tok.RefreshToken != "ref-1" || tok.ExpiresIn != 120 {
		t.Fatalf("first step: %d %s", status, body)
	}
	if status, body = tokenCall(t, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"r1"}}); status != 400 || !strings.Contains(body, `"invalid_grant"`) {
		t.Fatalf("second step: %d %s", status, body)
	}
	reqs := p.Requests()
	if len(reqs) != 2 || reqs[0].GrantType != "refresh_token" || reqs[1].Form.Get("refresh_token") != "r1" {
		t.Fatalf("requests: %+v", reqs)
	}
	id, secret, ok := reqs[0].BasicAuth()
	if !ok || id != p.ClientID || secret != p.ClientSecret {
		t.Fatalf("basic auth decoded as %q/%q ok=%v", id, secret, ok)
	}
}

func TestOAuthProviderRejectsBadClientAuthWithoutConsumingTheScript(t *testing.T) {
	p := fakes.StartOAuthProvider(t)
	p.Script(fakes.Ok{AccessToken: "acc"})
	status, _, body := send(t, "POST", p.TokenURL, "grant_type=refresh_token&refresh_token=x",
		"Content-Type: application/x-www-form-urlencoded", "Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("a:b")))
	if status != 401 || !strings.Contains(body, "invalid_client") {
		t.Fatalf("bad client auth: %d %s", status, body)
	}
	if status, _ = tokenCall(t, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}}); status != 200 {
		t.Fatalf("the script step must still be there: %d", status)
	}
}

func TestOAuthProviderHangEndsWithTheCallersDeadline(t *testing.T) {
	p := fakes.StartOAuthProvider(t)
	p.Script(fakes.Hang{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader("grant_type=refresh_token"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))
	if resp, err := localClient().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a hanging provider must not answer")
	}
}

func TestOAuthProviderCodeFlowAndRevoke(t *testing.T) {
	p := fakes.StartOAuthProvider(t)
	verifier := "verifier-verifier-verifier-verifier-1234"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {p.ClientID}, "response_type": {"code"}, "state": {"s-9"},
		"redirect_uri":   {"http://localhost:3000/api/connections/callback"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	status, hdr, _ := send(t, "GET", p.AuthorizeURL+"?"+q.Encode(), "")
	loc, err := url.Parse(hdr.Get("Location"))
	if status != 302 || err != nil || loc.Query().Get("state") != "s-9" || loc.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %v", status, hdr)
	}
	status, body := tokenCall(t, p, url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {"http://localhost:3000/api/connections/callback"}, "code_verifier": {verifier},
	})
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(body), &tok); status != 200 || err != nil || tok.RefreshToken == "" {
		t.Fatalf("code exchange: %d %s", status, body)
	}
	// The issued refresh token works for a refresh; an unknown one does not.
	if status, _ = tokenCall(t, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}); status != 200 {
		t.Fatalf("refresh: %d", status)
	}
	if status, _ = tokenCall(t, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"unknown"}}); status != 400 {
		t.Fatalf("unknown refresh token: %d", status)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(p.ClientID) + ":" + url.QueryEscape(p.ClientSecret)))
	if status, _, _ = send(t, "POST", p.RevokeURL, "token=tok-to-revoke", "Content-Type: application/x-www-form-urlencoded", "Authorization: Basic "+basic); status != 200 {
		t.Fatalf("revoke: %d", status)
	}
	if r := p.Revoked(); len(r) != 1 || r[0] != "tok-to-revoke" {
		t.Fatalf("revoked: %v", r)
	}
}

func TestNamesHelper(t *testing.T) {
	if got := strings.Join(names(fakes.Tools("p", "q")), ","); got != "p,q" {
		t.Fatalf("Tools names: %s", got)
	}
}
