package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
)

const (
	// defaultTimeout bounds one request to the provider, the response body included. A refresh
	// runs under the connection's row lock, so this also bounds how long other replicas wait
	// (TOKEN_ENDPOINT_TIMEOUT_SEC of the TypeScript client).
	defaultTimeout = 10 * time.Second
	// maxResponseBytes caps what is read from the provider. The reference has no cap; a token
	// answer is a few hundred bytes.
	maxResponseBytes = 1 << 20
	// maxExpiresIn keeps now + expires_in inside what time.Duration and Postgres can hold. A
	// larger value (a provider's way of saying "never expires") is read as no expiry.
	maxExpiresIn = 100 * 365 * 24 * 3600.0
	userAgent    = "mcp-registry"
)

// OAuthClient is app.OAuthClient written on net/http: Authorization Code + PKCE (S256) without
// discovery (the admin enters the endpoints) and without ID tokens (the catalog does not allow the
// `openid` scope). It mirrors the reference's client (openid-client 6.8.8 over oauth4webapi):
//
//   - the client authenticates with HTTP Basic, client id and secret each form-encoded first
//     (RFC 6749 section 2.3.1, WHATWG serialiser: see formEncode), or, with no secret, puts
//     `client_id` in the body;
//   - no redirect is followed (a 3xx is an answer) and no proxy of the environment is used;
//   - the whole exchange, body included, has one timeout, and the response is capped;
//   - only HTTP 200 is a token answer; the failure classes are in tokenAnswer.
//
// Errors carry fixed texts: never a response body, a request body, a header, a token, a code, a
// state, a verifier, the client secret or a URL (a transport error quotes the URL, query included).
// Safe for concurrent use.
type OAuthClient struct {
	clock   kernel.Clock
	timeout time.Duration
	client  *http.Client
}

var _ app.OAuthClient = (*OAuthClient)(nil)

// NewOAuthClient builds the client. clock gives the receive time ExpiresAt is counted from (the
// application clock, as in the reference; nil is the system clock); timeout is the limit of one
// request (zero or negative: 10 s).
func NewOAuthClient(clock kernel.Clock, timeout time.Duration) *OAuthClient {
	if clock == nil {
		clock = kernel.SystemClock{}
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the default transport is an *http.Transport
	tr.Proxy = nil                                        // the reference's fetch ignores HTTP_PROXY and HTTPS_PROXY too
	tr.DisableCompression = true
	return &OAuthClient{
		clock:   clock,
		timeout: timeout,
		client: &http.Client{
			Transport: tr,
			// A redirect is the provider's answer; following it would send the client secret and
			// the codes wherever the Location header says.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

type oauthError struct{ msg string }

func (e *oauthError) Error() string { return e.msg }

var (
	errNoAnswer       = &oauthError{"the OAuth provider gave no usable answer"}
	errEndpointURL    = &oauthError{"an OAuth endpoint URL is not valid"}
	errEndpointScheme = &oauthError{"an OAuth endpoint must use HTTPS"}
	errEndpointCreds  = &oauthError{"an OAuth endpoint URL must not contain credentials"}
	errNoRevokeURL    = &oauthError{"the server has no revoke URL"}
	errEmptyInput     = &oauthError{"a required OAuth request value is empty"}
	errClash          = &oauthError{"an extra authorize parameter clashes with a parameter of the flow"}
	errRevoke         = &oauthError{"the revocation endpoint did not answer HTTP 200"}
)

// endpointPolicy is what the reference derives from the TOKEN URL alone: plain http is allowed for
// every endpoint of the server (authorize, token, revoke) only when the token URL is http.
type endpointPolicy struct{ allowInsecure bool }

func policyOf(cfg catalog.OAuthClientConfig) (endpointPolicy, error) {
	u, err := parseEndpoint(cfg.TokenURL)
	if err != nil {
		return endpointPolicy{}, err
	}
	return endpointPolicy{allowInsecure: u.Scheme == "http"}, nil
}

// parseEndpoint is `new URL(s)` as far as it matters: an absolute http(s) URL with a host.
func parseEndpoint(s string) (*neturl.URL, error) {
	u, err := neturl.Parse(s)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, errEndpointURL
	}
	return u, nil
}

// checkProtocol is oauth4webapi's checkProtocol: https unless insecure requests are allowed, and
// nothing but http(s) ever.
func (p endpointPolicy) check(u *neturl.URL) error {
	if u.Scheme != "https" && (!p.allowInsecure || u.Scheme != "http") {
		return errEndpointScheme
	}
	return nil
}

// reservedAuthorizeParams are set by the flow itself; the catalog already forbids them as extras.
var reservedAuthorizeParams = map[string]bool{
	"response_type": true, "client_id": true, "redirect_uri": true, "scope": true,
	"state": true, "code_challenge": true, "code_challenge_method": true,
}

// AuthorizeURL builds the provider URL as oidc.buildAuthorizationUrl does: the configured endpoint
// keeps its own query (re-serialised as form data), then the extras, `response_type`,
// `redirect_uri`, `state`, `code_challenge`, `code_challenge_method`, `scope` (only with scopes,
// space-joined) and `client_id` are appended in that order. An extra that names one of the flow's
// own parameters is refused: the catalog does not allow it, this is the second line.
func (c *OAuthClient) AuthorizeURL(cfg catalog.OAuthClientConfig, in app.AuthorizeInput) (string, error) {
	policy, err := policyOf(cfg)
	if err != nil {
		return "", err
	}
	u, err := parseEndpoint(cfg.AuthorizeURL)
	if err != nil {
		return "", err
	}
	if err := policy.check(u); err != nil {
		return "", err
	}
	if u.User != nil {
		return "", errEndpointCreds
	}
	var pairs [][2]string
	for _, p := range cfg.ExtraAuthorizeParams {
		if reservedAuthorizeParams[p.Key] {
			return "", errClash
		}
		pairs = append(pairs, [2]string{p.Key, p.Value})
	}
	pairs = append(pairs,
		[2]string{"response_type", "code"},
		[2]string{"redirect_uri", in.RedirectURI},
		[2]string{"state", in.State.Reveal()},
		[2]string{"code_challenge", in.CodeChallenge},
		[2]string{"code_challenge_method", "S256"},
	)
	if len(cfg.Scopes) > 0 {
		pairs = append(pairs, [2]string{"scope", strings.Join(cfg.Scopes, " ")})
	}
	pairs = append(pairs, [2]string{"client_id", cfg.ClientID})

	query := existingQuery(u.RawQuery)
	if added := formBody(pairs); query == "" {
		query = added
	} else {
		query += "&" + added
	}
	u.RawQuery = query
	return u.String(), nil
}

// existingQuery is what `url.searchParams.append` leaves of a query already on the URL: parsed
// and serialised again as form data.
func existingQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var pairs [][2]string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{formDecode(k), formDecode(v)})
	}
	return formBody(pairs)
}

// ExchangeCode swaps the code for tokens: grant_type=authorization_code with `redirect_uri`,
// `code` and `code_verifier` (left out when there is none, as the reference does). No `state` is
// sent: it was matched against the pending attempt already.
func (c *OAuthClient) ExchangeCode(ctx context.Context, cfg catalog.OAuthClientConfig, secret app.ClientSecret, in app.ExchangeInput) (app.TokenSet, error) {
	if in.Code.Reveal() == "" || in.RedirectURI == "" {
		return app.TokenSet{}, errEmptyInput
	}
	pairs := [][2]string{{"redirect_uri", in.RedirectURI}, {"code", in.Code.Reveal()}}
	if in.CodeVerifier.Reveal() != "" {
		pairs = append(pairs, [2]string{"code_verifier", in.CodeVerifier.Reveal()})
	}
	pairs = append(pairs, [2]string{"grant_type", "authorization_code"})
	return c.grant(ctx, cfg, secret, pairs)
}

// Refresh is grant_type=refresh_token with the refresh token (no `scope`). It returns
// *app.TokenEndpointError when the provider answered, an error of another type for no answer.
func (c *OAuthClient) Refresh(ctx context.Context, cfg catalog.OAuthClientConfig, secret app.ClientSecret, refreshToken kernel.Secret) (app.TokenSet, error) {
	if refreshToken.Reveal() == "" {
		return app.TokenSet{}, errEmptyInput
	}
	return c.grant(ctx, cfg, secret, [][2]string{{"refresh_token", refreshToken.Reveal()}, {"grant_type", "refresh_token"}})
}

// Revoke is RFC 7009: POST `token=<token>` (no token_type_hint, as the reference) to the revoke
// URL with the same client authentication; only HTTP 200 counts.
func (c *OAuthClient) Revoke(ctx context.Context, cfg catalog.OAuthClientConfig, secret app.ClientSecret, token kernel.Secret) error {
	if token.Reveal() == "" {
		return errEmptyInput
	}
	if cfg.RevokeURL == nil {
		return errNoRevokeURL
	}
	policy, err := policyOf(cfg)
	if err != nil {
		return err
	}
	res, err := c.post(ctx, policy, *cfg.RevokeURL, cfg, secret, [][2]string{{"token", token.Reveal()}}, false)
	if err != nil {
		return err
	}
	if res.status != http.StatusOK {
		return errRevoke
	}
	return nil
}

func (c *OAuthClient) grant(ctx context.Context, cfg catalog.OAuthClientConfig, secret app.ClientSecret, pairs [][2]string) (app.TokenSet, error) {
	policy, err := policyOf(cfg)
	if err != nil {
		return app.TokenSet{}, err
	}
	res, err := c.post(ctx, policy, cfg.TokenURL, cfg, secret, pairs, true)
	if err != nil {
		return app.TokenSet{}, err
	}
	received := c.clock.Now()
	tok, err := tokenAnswer(res)
	if err != nil {
		return app.TokenSet{}, err
	}
	ts := app.TokenSet{AccessToken: tok.access, RefreshToken: tok.refresh}
	if tok.expiresIn != nil {
		at := received.Add(time.Duration(*tok.expiresIn) * time.Second)
		ts.ExpiresAt = &at
	}
	return ts, nil
}

// response is an answer of the provider. A token answer's body carries the tokens, so it is held
// as a kernel.SecretBytes: printing a response shows no token.
type response struct {
	status int
	header http.Header
	body   kernel.SecretBytes
}

// post sends one form request with the client's authentication and reads the whole answer, under
// one timeout. A request that fails before an answer is complete is "no answer".
func (c *OAuthClient) post(ctx context.Context, policy endpointPolicy, rawURL string, cfg catalog.OAuthClientConfig, secret app.ClientSecret, pairs [][2]string, wantJSON bool) (response, error) {
	u, err := parseEndpoint(rawURL)
	if err != nil {
		return response{}, err
	}
	if err := policy.check(u); err != nil {
		return response{}, err
	}
	// Go would send the URL's userinfo as Basic authentication; the reference's fetch refuses the
	// URL altogether.
	if u.User != nil {
		return response{}, errEndpointCreds
	}
	value, hasSecret := secret.Value()
	if !hasSecret {
		pairs = append(append([][2]string(nil), pairs...), [2]string{"client_id", cfg.ClientID})
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, rawURL, strings.NewReader(formBody(pairs)))
	if err != nil {
		return response{}, errEndpointURL
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	if wantJSON {
		req.Header.Set("Accept", "application/json")
	} else {
		req.Header.Set("Accept", "*/*") // fetch's default, which the reference's revocation request carries
	}
	req.Header.Set("User-Agent", userAgent)
	if hasSecret {
		req.Header.Set("Authorization", basicHeader(cfg.ClientID, value))
	}
	res, err := c.client.Do(req)
	if err != nil {
		return response{}, c.failure(ctx)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return response{}, c.failure(ctx)
	}
	return response{status: res.StatusCode, header: res.Header, body: kernel.NewSecretBytes(body)}, nil
}

// failure is what a request that did not complete reports: the caller's own cancellation as the
// context error, anything else (timeout, network, a body over the cap) as "no answer".
func (c *OAuthClient) failure(caller context.Context) error {
	if err := caller.Err(); err != nil {
		return err
	}
	return errNoAnswer
}

// tokens is what a token answer gave: the tokens as kernel.Secret (refresh unset when there was
// none).
type tokens struct {
	access, refresh kernel.Secret
	expiresIn       *float64 // whole seconds
}

// tokenAnswer sorts an answer of the token endpoint the way the reference does:
//
//   - 200: a JSON object (any Content-Type, as oauth4webapi checks the type only when parsing
//     fails) with a non-empty string `access_token`, a non-empty string `token_type` that is
//     "bearer" or "dpop" in any case, `expires_in` (when present) a number or a string parseFloat
//     reads, finite and not negative, `refresh_token` (when present) a non-empty string and
//     `scope` (when present) a string. Anything else is "no answer". An `id_token` is ignored: no
//     ID token is ever used here (the reference tries to validate it and fails the exchange).
//   - any other status is an answer of the endpoint, *app.TokenEndpointError: a WWW-Authenticate
//     challenge that parses decides the code (its `error`, empty when it has none) over the body;
//     else a 4xx with Content-Type exactly application/json (parameters allowed) and a JSON object
//     with a non-empty string `error` gives that code; every other case (non-JSON, 3xx, 5xx, 2xx
//     other than 200) has the status and no code.
func tokenAnswer(res response) (tokens, error) {
	if res.status != http.StatusOK {
		return tokens{}, &app.TokenEndpointError{Status: res.status, Code: errorCode(res)}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(stripBOM(res.body.Reveal()), &m); err != nil || m == nil {
		return tokens{}, errNoAnswer
	}
	var t tokens
	access, ok := nonEmptyString(m["access_token"])
	if !ok {
		return tokens{}, errNoAnswer
	}
	t.access = kernel.NewSecret(access)
	tokenType, ok := nonEmptyString(m["token_type"])
	if !ok {
		return tokens{}, errNoAnswer
	}
	if lt := strings.ToLower(tokenType); lt != "bearer" && lt != "dpop" {
		return tokens{}, errNoAnswer
	}
	if raw, present := m["expires_in"]; present {
		secs, ok := parseExpiresIn(raw)
		if !ok {
			return tokens{}, errNoAnswer
		}
		// A value beyond what a time can hold is a provider's way of saying "never": no expiry.
		if secs <= maxExpiresIn {
			t.expiresIn = &secs
		}
	}
	if raw, present := m["refresh_token"]; present {
		refresh, ok := nonEmptyString(raw)
		if !ok {
			return tokens{}, errNoAnswer
		}
		t.refresh = kernel.NewSecret(refresh)
	}
	if raw, present := m["scope"]; present {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return tokens{}, errNoAnswer
		}
	}
	return t, nil
}

// stripBOM drops one leading UTF-8 byte order mark, as fetch's response.json() does.
func stripBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) }

func nonEmptyString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

var floatPrefixRE = regexp.MustCompile(`^[ \t\n\r\f\v]*[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?`)

// parseExpiresIn reads `expires_in` as oauth4webapi does (a number as it is, a string with
// parseFloat) and returns whole seconds (the reference's expiresIn() rounds down).
func parseExpiresIn(raw json.RawMessage) (float64, bool) {
	var f float64
	switch {
	case len(raw) > 0 && raw[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		m := floatPrefixRE.FindString(s)
		if m == "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(strings.TrimLeft(m, " \t\n\r\f\v"), 64)
		if err != nil {
			return 0, false
		}
		f = v
	case len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9'):
		v, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return 0, false
		}
		f = v
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return math.Floor(f), true
}

// errorCode is the `error` of a non-200 answer, "" when there is none (see tokenAnswer).
func errorCode(res response) string {
	if challenges := parseWWWAuthenticate(strings.Join(res.header.Values("Www-Authenticate"), ", ")); challenges != nil {
		return challenges[0].params["error"]
	}
	if res.status < 400 || res.status > 499 {
		return ""
	}
	if ct, _, _ := strings.Cut(res.header.Get("Content-Type"), ";"); ct != "application/json" {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(stripBOM(bytes.TrimSpace(res.body.Reveal())), &m) != nil || m == nil {
		return ""
	}
	code, _ := nonEmptyString(m["error"])
	return code
}
