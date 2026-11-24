package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

var clientNow = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// recorded is one request the provider received.
type recorded struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   string
}

// provider is a recording OAuth provider: every request is kept, the answer is scripted.
type provider struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	answer func(w http.ResponseWriter, r *http.Request)
}

func newProvider(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *provider {
	t.Helper()
	p := &provider{answer: answer}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.reqs = append(p.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		p.mu.Unlock()
		p.answer(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *provider) requests() []recorded {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recorded(nil), p.reqs...)
}

func okJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func status(code int, contentType, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func testClient() *OAuthClient {
	return NewOAuthClient(kernel.FixedClock{T: clientNow}, 10*time.Second)
}

func cfgFor(base string) catalog.OAuthClientConfig {
	revoke := base + "/revoke"
	return catalog.OAuthClientConfig{
		AuthorizeURL: base + "/authorize", TokenURL: base + "/token", ClientID: "gateway-client",
		Scopes: []string{"repo"}, RevokeURL: &revoke,
	}
}

const refKey = "gate+way%2F:secret~*"

func headerNames(h http.Header) []string {
	var n []string
	for k := range h {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// The expected bytes below are what the reference's client (openid-client 6.8.8 over oauth4webapi
// 3.8.8) put on the wire in a capture against a raw socket, with this client id and secret.
const refBasic = "Basic Z2F0ZXdheS1jbGllbnQ6Z2F0ZSUyQndheSUyNTJGJTNBc2VjcmV0JTdFKg=="

func TestExchangeRequestMatchesTheReference(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer","expires_in":3600,"refresh_token":"r"}`))
	ts, err := testClient().ExchangeCode(context.Background(), cfgFor(p.URL), app.NewClientSecret(refKey),
		app.ExchangeInput{Code: kernel.NewSecret("the code&1"), CodeVerifier: kernel.NewSecret("verifier-123"), RedirectURI: "http://gw.test/api/connections/callback"})
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken.Reveal() != "a" || ts.RefreshToken.Reveal() != "r" || ts.ExpiresAt == nil || !ts.ExpiresAt.Equal(clientNow.Add(time.Hour)) {
		t.Fatalf("tokens: %v exp %v", ts.AccessToken, ts.ExpiresAt)
	}
	reqs := p.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/token" || r.RawQuery != "" {
		t.Errorf("request line: %s %s?%s", r.Method, r.Path, r.RawQuery)
	}
	wantBody := "redirect_uri=http%3A%2F%2Fgw.test%2Fapi%2Fconnections%2Fcallback&code=the+code%261&code_verifier=verifier-123&grant_type=authorization_code"
	if r.Body != wantBody {
		t.Errorf("body\n got %s\nwant %s", r.Body, wantBody)
	}
	for k, want := range map[string]string{
		"Accept":        "application/json",
		"Authorization": refBasic,
		"Content-Type":  "application/x-www-form-urlencoded;charset=UTF-8",
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, want := headerNames(r.Header), []string{"Accept", "Authorization", "Content-Length", "Content-Type", "User-Agent"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("header set %v, want %v", got, want)
	}
}

func TestRefreshRequestMatchesTheReference(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"a","token_type":"Bearer"}`))
	ts, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret(refKey), kernel.NewSecret("refresh token/1"))
	if err != nil {
		t.Fatal(err)
	}
	if ts.RefreshToken.Reveal() != "" || ts.ExpiresAt != nil {
		t.Fatalf("no refresh token and no expiry in the answer: %v %v", ts.RefreshToken, ts.ExpiresAt)
	}
	r := p.requests()[0]
	if r.Body != "refresh_token=refresh+token%2F1&grant_type=refresh_token" {
		t.Errorf("body %s", r.Body)
	}
	if r.Header.Get("Authorization") != refBasic || r.Header.Get("Accept") != "application/json" {
		t.Errorf("headers %v", r.Header)
	}
}

func TestPublicClientSendsClientIDInTheBodyAndNoAuthorization(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer"}`))
	c := testClient()
	if _, err := c.Refresh(context.Background(), cfgFor(p.URL), app.ClientSecret{}, kernel.NewSecret("r2")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExchangeCode(context.Background(), cfgFor(p.URL), app.ClientSecret{}, app.ExchangeInput{Code: kernel.NewSecret("c"), CodeVerifier: kernel.NewSecret("v"), RedirectURI: "http://gw.test/cb"}); err != nil {
		t.Fatal(err)
	}
	reqs := p.requests()
	if reqs[0].Body != "refresh_token=r2&grant_type=refresh_token&client_id=gateway-client" {
		t.Errorf("refresh body %s", reqs[0].Body)
	}
	if reqs[1].Body != "redirect_uri=http%3A%2F%2Fgw.test%2Fcb&code=c&code_verifier=v&grant_type=authorization_code&client_id=gateway-client" {
		t.Errorf("exchange body %s", reqs[1].Body)
	}
	for _, r := range reqs {
		if _, has := r.Header["Authorization"]; has {
			t.Errorf("a public client sent Authorization: %v", r.Header)
		}
	}
}

func TestExchangeWithoutVerifierSendsNoCodeVerifier(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer"}`))
	if _, err := testClient().ExchangeCode(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), app.ExchangeInput{Code: kernel.NewSecret("c"), RedirectURI: "http://gw.test/cb"}); err != nil {
		t.Fatal(err)
	}
	if b := p.requests()[0].Body; strings.Contains(b, "code_verifier") {
		t.Errorf("body %s", b)
	}
}

func TestRevokeRequestMatchesTheReference(t *testing.T) {
	p := newProvider(t, status(200, "", ""))
	if err := testClient().Revoke(context.Background(), cfgFor(p.URL), app.NewClientSecret(refKey), kernel.NewSecret("refresh token/1")); err != nil {
		t.Fatal(err)
	}
	r := p.requests()[0]
	if r.Method != "POST" || r.Path != "/revoke" || r.Body != "token=refresh+token%2F1" {
		t.Errorf("%s %s %q", r.Method, r.Path, r.Body)
	}
	if r.Header.Get("Authorization") != refBasic || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded;charset=UTF-8" {
		t.Errorf("headers %v", r.Header)
	}
	if got := r.Header.Get("Accept"); got != "*/*" {
		t.Errorf("Accept = %q: the reference sends no Accept of its own, fetch fills in */*", got)
	}
}

func TestRevokeAnswersAreErrorsExceptHTTP200(t *testing.T) {
	for _, code := range []int{200, 201, 204, 302, 400, 401, 500} {
		p := newProvider(t, status(code, "", ""))
		err := testClient().Revoke(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("tok"))
		if (code == 200) != (err == nil) {
			t.Errorf("HTTP %d: err = %v", code, err)
		}
	}
	cfg := cfgFor("http://127.0.0.1:1")
	cfg.RevokeURL = nil
	if err := testClient().Revoke(context.Background(), cfg, app.NewClientSecret("s"), kernel.NewSecret("tok")); err == nil {
		t.Error("a server without a revoke URL must not revoke")
	}
}

func TestBasicHeaderOfTheClientUsesFormEncodingForStarAndTilde(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer"}`))
	cfg := cfgFor(p.URL)
	cfg.ClientID = "id*~ x"
	if _, err := testClient().Refresh(context.Background(), cfg, app.NewClientSecret("s*~ é"), kernel.NewSecret("r")); err != nil {
		t.Fatal(err)
	}
	if got, want := p.requests()[0].Header.Get("Authorization"), basicHeader("id*~ x", "s*~ é"); got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

// answerCase is one response class of the token endpoint.
type answerCase struct {
	name   string
	answer func(http.ResponseWriter, *http.Request)
	// want: "ok", "none" (no answer) or "HTTP <status> <code>".
	want string
}

func jsonAt(code int, ct, body string) func(http.ResponseWriter, *http.Request) {
	return status(code, ct, body)
}

func responseClasses() []answerCase {
	const j = "application/json"
	return []answerCase{
		{"200 full", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":3600,"refresh_token":"r"}`), "ok"},
		{"200 content type is not looked at when the body parses", jsonAt(200, "text/plain", `{"access_token":"a","token_type":"bearer"}`), "ok"},
		{"200 expires_in as a string", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":"3600"}`), "ok"},
		{"200 expires_in zero", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":0}`), "ok"},
		{"200 expires_in fractional", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":3600.9}`), "ok"},
		{"200 token type DPoP", jsonAt(200, j, `{"access_token":"a","token_type":"DPoP"}`), "ok"},
		{"200 extra members and id_token are ignored", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","scope":"x","id_token":"e30.e30.e30","x":[1]}`), "ok"},
		{"200 expires_in negative", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":-1}`), "none"},
		{"200 expires_in text", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":"soon"}`), "none"},
		{"200 expires_in null", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":null}`), "none"},
		{"200 expires_in huge means no expiry", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","expires_in":1e300}`), "ok"},
		{"200 with a byte order mark", jsonAt(200, j, "\ufeff"+`{"access_token":"a","token_type":"bearer"}`), "ok"},
		{"400 invalid_client with a byte order mark", jsonAt(400, j, "\ufeff"+`{"error":"invalid_client"}`), "HTTP 400 invalid_client"},
		{"400 invalid_grant with a byte order mark", jsonAt(400, j, "\ufeff"+`{"error":"invalid_grant"}`), "HTTP 400 invalid_grant"},
		{"401 invalid_client with a byte order mark", jsonAt(401, j, "\ufeff"+`{"error":"invalid_client"}`), "HTTP 401 invalid_client"},
		{"200 no access_token", jsonAt(200, j, `{"token_type":"bearer"}`), "none"},
		{"200 empty access_token", jsonAt(200, j, `{"access_token":"","token_type":"bearer"}`), "none"},
		{"200 numeric access_token", jsonAt(200, j, `{"access_token":5,"token_type":"bearer"}`), "none"},
		{"200 no token_type", jsonAt(200, j, `{"access_token":"a"}`), "none"},
		{"200 token type mac", jsonAt(200, j, `{"access_token":"a","token_type":"mac"}`), "none"},
		{"200 empty refresh_token", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","refresh_token":""}`), "none"},
		{"200 numeric refresh_token", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","refresh_token":1}`), "none"},
		{"200 numeric scope", jsonAt(200, j, `{"access_token":"a","token_type":"bearer","scope":1}`), "none"},
		{"200 array body", jsonAt(200, j, `[]`), "none"},
		{"200 null body", jsonAt(200, j, `null`), "none"},
		{"200 empty body", jsonAt(200, j, ``), "none"},
		{"200 html", jsonAt(200, "text/html", `<html>login</html>`), "none"},
		{"201 is not a token response", jsonAt(201, j, `{"access_token":"a","token_type":"bearer"}`), "HTTP 201"},
		{"204", jsonAt(204, "", ``), "HTTP 204"},
		{"400 invalid_grant", jsonAt(400, j, `{"error":"invalid_grant","error_description":"gone"}`), "HTTP 400 invalid_grant"},
		{"400 json with a charset", jsonAt(400, j+"; charset=utf-8", `{"error":"invalid_request"}`), "HTTP 400 invalid_request"},
		{"401 invalid_client", jsonAt(401, j, `{"error":"invalid_client"}`), "HTTP 401 invalid_client"},
		{"403 invalid_grant", jsonAt(403, j, `{"error":"invalid_grant"}`), "HTTP 403 invalid_grant"},
		{"429 with an error", jsonAt(429, j, `{"error":"slow_down"}`), "HTTP 429 slow_down"},
		{"400 content type compared exactly (case)", jsonAt(400, "Application/JSON", `{"error":"invalid_grant"}`), "HTTP 400"},
		{"400 text/plain", jsonAt(400, "text/plain", `{"error":"invalid_grant"}`), "HTTP 400"},
		{"400 no content type", jsonAt(400, "", `{"error":"invalid_grant"}`), "HTTP 400"},
		{"400 html", jsonAt(400, "text/html", `<html>bad</html>`), "HTTP 400"},
		{"400 json without error", jsonAt(400, j, `{"message":"bad"}`), "HTTP 400"},
		{"400 empty error", jsonAt(400, j, `{"error":""}`), "HTTP 400"},
		{"400 numeric error", jsonAt(400, j, `{"error":7}`), "HTTP 400"},
		{"400 array body", jsonAt(400, j, `["invalid_grant"]`), "HTTP 400"},
		{"400 broken json", jsonAt(400, j, `{"error":`), "HTTP 400"},
		{"500 json error is not looked at", jsonAt(500, j, `{"error":"server_error"}`), "HTTP 500"},
		{"503 json error is not looked at", jsonAt(503, j, `{"error":"temporarily_unavailable"}`), "HTTP 503"},
		{"502 html", jsonAt(502, "text/html", `<html>bad gateway</html>`), "HTTP 502"},
		{"401 challenge wins over the body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.Header().Set("Content-Type", j)
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		}, "HTTP 401 invalid_token"},
		{"401 challenge without error hides the body's", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.Header().Set("Content-Type", j)
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		}, "HTTP 401"},
		{"500 challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer error="x"`)
			w.WriteHeader(500)
		}, "HTTP 500 x"},
		{"unparsable challenge falls through to the body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", `;;;`)
			w.Header().Set("Content-Type", j)
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		}, "HTTP 400 invalid_grant"},
		{"connection dropped", func(w http.ResponseWriter, _ *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
		}, "none"},
		{"truncated body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.Header().Set("Content-Type", j)
			_, _ = io.WriteString(w, `{"access_token":`)
		}, "none"},
	}
}

func describe(err error) string {
	if err == nil {
		return "ok"
	}
	var te *app.TokenEndpointError
	if errors.As(err, &te) {
		if te.Code == "" {
			return fmt.Sprintf("HTTP %d", te.Status)
		}
		return fmt.Sprintf("HTTP %d %s", te.Status, te.Code)
	}
	return "none"
}

func TestEveryResponseClassOfRefreshAndExchange(t *testing.T) {
	for _, tc := range responseClasses() {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, tc.answer)
			_, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
			if got := describe(err); got != tc.want {
				t.Errorf("Refresh: %s (%v), want %s", got, err, tc.want)
			}
			_, err = testClient().ExchangeCode(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), app.ExchangeInput{Code: kernel.NewSecret("c"), CodeVerifier: kernel.NewSecret("v"), RedirectURI: "http://gw.test/cb"})
			if got := describe(err); got != tc.want {
				t.Errorf("ExchangeCode: %s (%v), want %s", got, err, tc.want)
			}
		})
	}
}

func TestExpiresAtIsReceiveTimePlusExpiresIn(t *testing.T) {
	for _, tc := range []struct {
		body string
		want time.Duration
	}{
		{`"expires_in":3600`, time.Hour},
		{`"expires_in":"120"`, 2 * time.Minute},
		{`"expires_in":"90 seconds"`, 90 * time.Second}, // parseFloat reads the numeric prefix, as the reference
		{`"expires_in":0`, 0},
		{`"expires_in":1.9`, time.Second},
		{`"expires_in":1e2`, 100 * time.Second},
	} {
		p := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer",`+tc.body+`}`))
		ts, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
		if err != nil || ts.ExpiresAt == nil || !ts.ExpiresAt.Equal(clientNow.Add(tc.want)) {
			t.Errorf("%s: %v %v", tc.body, ts.ExpiresAt, err)
		}
	}
}

func TestRedirectsAreAnswersAndNeverFollowed(t *testing.T) {
	target := newProvider(t, okJSON(`{"access_token":"stolen","token_type":"bearer"}`))
	for _, code := range []int{301, 302, 303, 307, 308} {
		p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/token", code)
		})
		_, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
		if got := describe(err); got != fmt.Sprintf("HTTP %d", code) {
			t.Errorf("%d: %s", code, got)
		}
		if err := testClient().Revoke(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("tok")); err == nil {
			t.Errorf("%d: revoke through a redirect reported success", code)
		}
	}
	if n := len(target.requests()); n != 0 {
		t.Fatalf("the redirect target received %d requests (secrets would have gone with them)", n)
	}
}

func TestTheClientUsesNoProxyFromTheEnvironment(t *testing.T) {
	tr, ok := testClient().client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("transport %T, proxy set: %v", testClient().client.Transport, ok && tr.Proxy != nil)
	}
	if !tr.DisableCompression {
		t.Error("compression stays off: the request carries only the headers the reference's carries")
	}
}

func TestTimeoutCoversTheWholeExchangeIncludingTheBody(t *testing.T) {
	release := make(chan struct{})
	hang := newProvider(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	stall := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, `{"access_token":"`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	c := NewOAuthClient(kernel.FixedClock{T: clientNow}, 150*time.Millisecond)
	for name, p := range map[string]*provider{"no answer at all": hang, "body never finishes": stall} {
		start := time.Now()
		_, err := c.Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
		if describe(err) != "none" {
			t.Errorf("%s: %v", name, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %v", name, d)
		}
	}
}

func TestTheDefaultTimeoutIsTenSeconds(t *testing.T) {
	if c := NewOAuthClient(nil, 0); c.timeout != 10*time.Second {
		t.Errorf("timeout %v", c.timeout)
	}
}

func TestCallerCancellationIsReportedAsTheContextError(t *testing.T) {
	started := make(chan struct{})
	p := newProvider(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := testClient().Refresh(ctx, cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestResponseOver1MiBIsNoAnswer(t *testing.T) {
	big := `{"access_token":"` + strings.Repeat("a", 1<<20) + `","token_type":"bearer"}`
	p := newProvider(t, okJSON(big))
	_, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
	if describe(err) != "none" {
		t.Fatalf("err = %v", err)
	}
	small := `{"access_token":"` + strings.Repeat("a", 1<<19) + `","token_type":"bearer"}`
	p2 := newProvider(t, okJSON(small))
	if _, err := testClient().Refresh(context.Background(), cfgFor(p2.URL), app.NewClientSecret("s"), kernel.NewSecret("r")); err != nil {
		t.Fatalf("half a MiB is fine: %v", err)
	}
}

func TestEndpointRules(t *testing.T) {
	ok := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer"}`))
	revoke := newProvider(t, status(200, "", ""))
	c := testClient()
	cfgHTTPSToken := cfgFor(ok.URL)
	cfgHTTPSToken.TokenURL = "https://127.0.0.1:1/token" // https token endpoint: http revoke is forbidden
	cfgHTTPSToken.RevokeURL = ptr(revoke.URL + "/revoke")
	if err := c.Revoke(context.Background(), cfgHTTPSToken, app.NewClientSecret("s"), kernel.NewSecret("tok")); err == nil {
		t.Error("an https token URL must not allow an http revoke URL")
	}
	if n := len(revoke.requests()); n != 0 {
		t.Errorf("revoke sent %d requests to a forbidden endpoint", n)
	}
	// An http token URL allows plain http for all endpoints (the reference's allowInsecure).
	cfgHTTP := cfgFor(ok.URL)
	if err := c.Revoke(context.Background(), cfgHTTP, app.NewClientSecret("s"), kernel.NewSecret("tok")); err != nil && len(ok.requests()) == 0 {
		t.Errorf("http token URL: %v", err)
	}
	for name, endpoint := range map[string]string{ //nolint:gosec // G101: test data, no real credential (URLs and OAuth parameter names)
		"not a URL":       "::::",
		"ftp":             "ftp://127.0.0.1/token",
		"no host":         "http:///token",
		"credentials":     "http://user:pw@127.0.0.1:1/token",
		"empty":           "",
		"relative":        "/token",
		"opaque":          "mailto:a@b",
		"javascript like": "javascript:alert(1)",
	} {
		cfg := cfgFor(ok.URL)
		cfg.TokenURL = endpoint
		before := len(ok.requests())
		if _, err := c.Refresh(context.Background(), cfg, app.NewClientSecret("s"), kernel.NewSecret("r")); err == nil {
			t.Errorf("%s: no error", name)
		}
		if len(ok.requests()) != before {
			t.Errorf("%s: a request was sent", name)
		}
	}
	if _, err := c.Refresh(context.Background(), cfgFor(ok.URL), app.NewClientSecret("s"), kernel.NewSecret("")); err == nil {
		t.Error("an empty refresh token is refused before any request")
	}
	if _, err := c.ExchangeCode(context.Background(), cfgFor(ok.URL), app.NewClientSecret("s"), app.ExchangeInput{RedirectURI: "http://gw.test/cb"}); err == nil {
		t.Error("an empty code is refused before any request")
	}
}

func ptr[T any](v T) *T { return &v }

func TestAuthorizeURLMatchesTheReference(t *testing.T) {
	cfg := catalog.OAuthClientConfig{ //nolint:gosec // G101: test data, no real credential (URLs and OAuth parameter names)
		AuthorizeURL: "https://idp.example.com/authorize", TokenURL: "https://idp.example.com/token", ClientID: "gateway-client",
		Scopes: []string{"repo", "read:user"}, ExtraAuthorizeParams: []catalog.AuthorizeParam{{Key: "access_type", Value: "offline"}},
	}
	in := app.AuthorizeInput{RedirectURI: "http://gw.test/api/connections/callback", State: kernel.NewSecret("s t"), CodeChallenge: "ch"}
	got, err := testClient().AuthorizeURL(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	// Captured from oidc.buildAuthorizationUrl with the reference's parameter object.
	want := "https://idp.example.com/authorize?access_type=offline&response_type=code&redirect_uri=http%3A%2F%2Fgw.test%2Fapi%2Fconnections%2Fcallback&state=s+t&code_challenge=ch&code_challenge_method=S256&scope=repo+read%3Auser&client_id=gateway-client"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}

	cfg.Scopes = nil
	cfg.ExtraAuthorizeParams = nil
	got, err = testClient().AuthorizeURL(cfg, in)
	if err != nil || strings.Contains(got, "scope") || !strings.HasSuffix(got, "&client_id=gateway-client") {
		t.Errorf("no scopes: %s %v", got, err)
	}
}

func TestAuthorizeURLKeepsAndNormalisesTheConfiguredQuery(t *testing.T) {
	cfg := catalog.OAuthClientConfig{AuthorizeURL: "https://idp.example.com/a?tenant=my%20org&x=1#frag", TokenURL: "https://idp.example.com/token", ClientID: "c"} //nolint:gosec // G101: test data, no real credential
	got, err := testClient().AuthorizeURL(cfg, app.AuthorizeInput{RedirectURI: "r", State: kernel.NewSecret("s"), CodeChallenge: "c"})
	if err != nil {
		t.Fatal(err)
	}
	// new URL(...).searchParams.append re-serialises the whole query (space as +) and keeps the hash.
	want := "https://idp.example.com/a?tenant=my+org&x=1&response_type=code&redirect_uri=r&state=s&code_challenge=c&code_challenge_method=S256&client_id=c#frag"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestAuthorizeURLRefusesExtrasThatClashWithTheFlow(t *testing.T) {
	for _, key := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		cfg := catalog.OAuthClientConfig{ //nolint:gosec // G101: test data, no real credential
			AuthorizeURL: "https://idp.example.com/authorize", TokenURL: "https://idp.example.com/token", ClientID: "c",
			ExtraAuthorizeParams: []catalog.AuthorizeParam{{Key: key, Value: "evil"}},
		}
		got, err := testClient().AuthorizeURL(cfg, app.AuthorizeInput{RedirectURI: "r", State: kernel.NewSecret("STATEMARK"), CodeChallenge: "c"})
		if err == nil || got != "" {
			t.Errorf("%s: %q %v", key, got, err)
		} else if strings.Contains(err.Error(), "STATEMARK") {
			t.Errorf("%s: the error quotes the state: %v", key, err)
		}
	}
}

func TestAuthorizeURLSchemeRules(t *testing.T) {
	in := app.AuthorizeInput{RedirectURI: "r", State: kernel.NewSecret("s"), CodeChallenge: "c"}
	for _, tc := range []struct {
		name, authorize, token string
		ok                     bool
	}{
		{"https both", "https://a.example/authorize", "https://a.example/token", true},
		{"http both", "http://a.example/authorize", "http://a.example/token", true},
		{"https authorize, http token", "https://a.example/authorize", "http://a.example/token", true},
		{"http authorize, https token", "http://a.example/authorize", "https://a.example/token", false},
		{"ftp authorize, http token", "ftp://a.example/authorize", "http://a.example/token", false},
		{"invalid authorize", "::::", "https://a.example/token", false},
		{"invalid token", "https://a.example/authorize", "::::", false},
		{"relative authorize", "/authorize", "http://a.example/token", false},
	} {
		_, err := testClient().AuthorizeURL(catalog.OAuthClientConfig{AuthorizeURL: tc.authorize, TokenURL: tc.token, ClientID: "c"}, in)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestSecretsNeverReachAnErrorOrItsFormatting(t *testing.T) {
	const (
		code     = "CODEMARK-1"
		verifier = "VERIFIERMARK-2"
		secret   = "CLIENTSECRETMARK-3"
		refresh  = "REFRESHMARK-4"
		bodyMark = "BODYMARK-5"
		query    = "QUERYMARK-6"
	)
	markers := []string{code, verifier, secret, refresh, bodyMark, query, "Basic ", basicHeader("gateway-client", secret)[6:]}
	for _, tc := range responseClasses() {
		wrap := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Echo", bodyMark)
			tc.answer(w, r)
		}
		p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
			// Echo every secret in the body of error answers: the client must not copy a body.
			if tc.want != "ok" && !strings.HasPrefix(tc.want, "HTTP 2") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error_description":"`+bodyMark+" "+code+" "+refresh+" "+secret+" "+verifier+`"}`)
				return
			}
			wrap(w, r)
		})
		cfg := cfgFor(p.URL)
		cfg.TokenURL += "?" + query + "=1"
		cfg.RevokeURL = ptr(*cfg.RevokeURL + "?" + query + "=1")
		var errs []error
		_, e1 := testClient().Refresh(context.Background(), cfg, app.NewClientSecret(secret), kernel.NewSecret(refresh))
		_, e2 := testClient().ExchangeCode(context.Background(), cfg, app.NewClientSecret(secret), app.ExchangeInput{Code: kernel.NewSecret(code), CodeVerifier: kernel.NewSecret(verifier), RedirectURI: "http://gw.test/cb"})
		e3 := testClient().Revoke(context.Background(), cfg, app.NewClientSecret(secret), kernel.NewSecret(refresh))
		errs = append(errs, e1, e2, e3)
		// A dead endpoint: the transport's error quotes the URL (with its query).
		dead := cfgFor("http://127.0.0.1:1")
		dead.TokenURL += "?" + query + "=1"
		_, e4 := testClient().Refresh(context.Background(), dead, app.NewClientSecret(secret), kernel.NewSecret(refresh))
		errs = append(errs, e4)
		for _, err := range errs {
			if err == nil {
				continue
			}
			for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%s", err)} {
				for _, m := range markers {
					if strings.Contains(text, m) {
						t.Errorf("%s: error text %q contains %q", tc.name, text, m)
					}
				}
			}
		}
	}
}

func TestTokenSetNeverPrintsTokens(t *testing.T) {
	p := newProvider(t, okJSON(`{"access_token":"ACCESSMARK","token_type":"bearer","refresh_token":"REFRESHMARK"}`))
	ts, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v", ts), fmt.Sprintf("%+v", ts)} {
		if strings.Contains(s, "MARK") {
			t.Errorf("printed: %s", s)
		}
	}
}

func TestHugeExpiresInMeansNoExpiryAndAByteOrderMarkIsStripped(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"a","token_type":"bearer","expires_in":1e300}`,
		`{"access_token":"a","token_type":"bearer","expires_in":"99999999999999999999"}`,
		`{"access_token":"a","token_type":"bearer","expires_in":4000000000}`,
	} {
		p := newProvider(t, okJSON(body))
		ts, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
		if err != nil || ts.AccessToken.Reveal() != "a" || ts.ExpiresAt != nil {
			t.Errorf("%s: %v expiry %v", body, err, ts.ExpiresAt)
		}
	}
	p := newProvider(t, okJSON("\ufeff"+`{"access_token":"A","token_type":"bearer","expires_in":60}`))
	ts, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
	if err != nil || ts.AccessToken.Reveal() != "A" || ts.ExpiresAt == nil || !ts.ExpiresAt.Equal(clientNow.Add(time.Minute)) {
		t.Errorf("bom: %v %v", ts.ExpiresAt, err)
	}
}

func TestNoUsableAnswerNeverQuotesTheBody(t *testing.T) {
	const mark = "BODYMARK-XYZ"
	for _, body := range []string{
		`{"access_token":"` + mark + `","token_type":"mac"}`,
		`{"access_token":"` + mark + `"}`,
		`not json ` + mark,
		`{"access_token":"` + mark + `","token_type":"bearer","expires_in":"` + mark + `"}`,
	} {
		p := newProvider(t, okJSON(body))
		_, err := testClient().Refresh(context.Background(), cfgFor(p.URL), app.NewClientSecret("s"), kernel.NewSecret("r"))
		if err == nil {
			t.Fatalf("%s: no error", body)
		}
		for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(text, mark) {
				t.Errorf("error quotes the body: %s", text)
			}
		}
	}
}

func TestUserInfoInAnyEndpointIsRefusedBeforeAnythingIsSentAndNotQuoted(t *testing.T) {
	ok := newProvider(t, okJSON(`{"access_token":"a","token_type":"bearer"}`))
	host := strings.TrimPrefix(ok.URL, "http://")
	creds := "http://USERMARK:PWMARK@" + host
	c := testClient()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		for _, m := range []string{"USERMARK", "PWMARK"} {
			if strings.Contains(err.Error(), m) {
				t.Errorf("%s: error quotes the credentials: %v", what, err)
			}
		}
	}
	cfg := cfgFor(ok.URL)
	cfg.TokenURL = creds + "/token"
	_, err := c.Refresh(context.Background(), cfg, app.NewClientSecret("s"), kernel.NewSecret("r"))
	check("token", err)
	cfg = cfgFor(ok.URL)
	cfg.RevokeURL = ptr(creds + "/revoke")
	check("revoke", c.Revoke(context.Background(), cfg, app.NewClientSecret("s"), kernel.NewSecret("t")))
	cfg = cfgFor(ok.URL)
	cfg.AuthorizeURL = creds + "/authorize"
	_, err = c.AuthorizeURL(cfg, app.AuthorizeInput{RedirectURI: "r", State: kernel.NewSecret("s"), CodeChallenge: "c"})
	check("authorize", err)
	if n := len(ok.requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}
