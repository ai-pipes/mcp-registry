//go:build compat

package fakes

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TokenStep is one scripted answer of the token endpoint: Ok, Error, Hang or Drop.
type TokenStep interface{ tokenStep() }

// Ok is a successful token response. Empty fields get defaults: a fresh access token, token type
// "bearer" and expires_in 3600 (a negative ExpiresIn leaves expires_in out). An empty
// RefreshToken leaves refresh_token out, as Hub and Gitea do when they do not rotate it.
type Ok struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
}

// Error is an OAuth error answer: the status and {"error": Code}. Status 0 means 400.
type Error struct {
	Status int
	Code   string
}

// Hang never answers; the wait ends when the caller gives up or the test ends.
type Hang struct{}

// Drop closes the connection without any answer, as a network failure does.
type Drop struct{}

func (Drop) tokenStep()  {}
func (Ok) tokenStep()    {}
func (Error) tokenStep() {}
func (Hang) tokenStep()  {}

// RevokeAttempt is a request to the revocation endpoint, accepted or not.
type RevokeAttempt struct {
	Token         string
	Authorization string // the Authorization header as sent
	Accepted      bool   // the client authentication was valid
}

// TokenRequest is a request the token endpoint received.
type TokenRequest struct {
	GrantType     string
	Authorization string     // the Authorization header as sent
	Form          url.Values // the parsed form body
	Body          string     // the raw body
}

// BasicAuth decodes the Authorization header as RFC 6749 section 2.3.1 says (base64, split at the
// first colon, both halves form-decoded).
func (r TokenRequest) BasicAuth() (id, secret string, ok bool) {
	h, found := strings.CutPrefix(r.Authorization, "Basic ")
	if !found {
		return "", "", false
	}
	return decodeBasic(h)
}

// IssuedTokens are the tokens an authorization_code exchange handed out, in the order of the
// exchanges, so a test knows which token belongs to which connect.
type IssuedTokens struct {
	AccessToken  string
	RefreshToken string
}

// OAuthProvider is an OAuth 2.0 provider of the Gitea / YouTrack Hub kind: authorize (redirects
// back with a code at once), a token endpoint (authorization_code with PKCE S256, refresh_token)
// with HTTP Basic client authentication, and RFC 7009 revocation. Without a script it answers
// like a real provider: a code is exchanged once, a refresh token it issued is accepted and not
// rotated, any other one gets invalid_grant. The client secret holds characters that break a
// client that does not form-encode it before Basic.
type OAuthProvider struct {
	AuthorizeURL string
	TokenURL     string
	RevokeURL    string
	ClientID     string
	ClientSecret string

	mu         sync.Mutex
	script     []TokenStep
	requests   []TokenRequest
	revoked    []string
	attempts   []RevokeAttempt
	authorizes []url.Values
	issued     []IssuedTokens
	handed     []string
	refreshLag time.Duration
	grants     map[string]oauthGrant
	refreshes  map[string]bool
	stopped    chan struct{}
}

type oauthGrant struct{ challenge, redirectURI string }

// StartOAuthProvider starts a provider that stops with the test.
func StartOAuthProvider(t *testing.T) *OAuthProvider {
	t.Helper()
	p := &OAuthProvider{ //nolint:gosec // G101: a made-up client secret of a test double
		ClientID:     "gateway-client",
		ClientSecret: "gate+way%2F:secret",
		grants:       map[string]oauthGrant{},
		refreshes:    map[string]bool{},
		stopped:      make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("POST /revoke", p.revoke)
	base := baseURL(serve(t, mux))
	p.AuthorizeURL, p.TokenURL, p.RevokeURL = base+"/authorize", base+"/token", base+"/revoke"
	t.Cleanup(func() { close(p.stopped) })
	return p
}

// Script queues answers for refresh_token requests. A step is used only by a request that has
// valid client authentication and presents a refresh token this provider issued and that is still
// current; every other request is judged by the provider's own rules and leaves the queue alone,
// and authorization_code exchanges are never scripted. Ok only shapes the success body (a
// RefreshToken in it rotates the presented one); Error and Hang replace the success. Once the
// queue is empty the provider answers by its own rules again.
func (p *OAuthProvider) Script(steps ...TokenStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append(p.script, steps...)
}

// Requests returns every request the token endpoint received, in order.
func (p *OAuthProvider) Requests() []TokenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TokenRequest(nil), p.requests...)
}

// Revoked returns the tokens of revocation requests with valid client authentication, in order.
func (p *OAuthProvider) Revoked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.revoked...)
}

// RevokeAttempts returns every request to the revocation endpoint, including those refused for
// bad client authentication.
func (p *OAuthProvider) RevokeAttempts() []RevokeAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]RevokeAttempt(nil), p.attempts...)
}

// Issued returns the tokens of every successful authorization_code exchange, in order.
func (p *OAuthProvider) Issued() []IssuedTokens {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]IssuedTokens(nil), p.issued...)
}

// HandedOut returns every access and refresh token the provider ever put into a successful token
// response (code exchanges and refreshes, scripted or not), in order. A test uses it to look for
// leaks of any token, including those of refreshes.
func (p *OAuthProvider) HandedOut() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.handed...)
}

// SetRefreshDelay holds the answer of every refresh_token request that passed the provider's own
// checks (client authentication, a current refresh token) for d before it is sent, so concurrent
// callers overlap. A caller that gives up ends the wait. Zero removes the delay.
func (p *OAuthProvider) SetRefreshDelay(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLag = d
}

// AuthorizeRequests returns the query of every authorize request, in order.
func (p *OAuthProvider) AuthorizeRequests() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.authorizes...)
}

// authorize is the browser step: the person is signed in and consents at once.
func (p *OAuthProvider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || redirect == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	code := randomToken("code-")
	p.mu.Lock()
	p.authorizes = append(p.authorizes, q)
	p.grants[code] = oauthGrant{challenge: q.Get("code_challenge"), redirectURI: redirect}
	p.mu.Unlock()
	rq := target.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	target.RawQuery = rq.Encode()
	// The fake exists to send the browser to whatever redirect_uri the application registered.
	http.Redirect(w, r, target.String(), http.StatusFound) //nolint:gosec // G710: see above
}

func (p *OAuthProvider) clientOK(r *http.Request) bool {
	id, secret, ok := decodeBasic(trimBasic(r.Header.Get("Authorization")))
	return ok && id == p.ClientID && secret == p.ClientSecret
}

func trimBasic(h string) string {
	s, _ := strings.CutPrefix(h, "Basic ")
	return s
}

func (p *OAuthProvider) token(w http.ResponseWriter, r *http.Request) {
	raw := readBody(r)
	form, _ := url.ParseQuery(raw)
	req := TokenRequest{GrantType: form.Get("grant_type"), Authorization: r.Header.Get("Authorization"), Form: form, Body: raw}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if !p.clientOK(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		p.codeGrant(w, form)
	case "refresh_token":
		p.refreshGrant(w, r, form)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func invalidGrant(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
}

// codeGrant exchanges a code once; the script never applies to it.
func (p *OAuthProvider) codeGrant(w http.ResponseWriter, form url.Values) {
	code := form.Get("code")
	p.mu.Lock()
	grant, ok := p.grants[code]
	delete(p.grants, code)
	p.mu.Unlock()
	if !ok || pkceChallenge(form.Get("code_verifier")) != grant.challenge || form.Get("redirect_uri") != grant.redirectURI {
		invalidGrant(w)
		return
	}
	refresh, access := randomToken("refresh-"), randomToken("access-")
	p.mu.Lock()
	p.refreshes[refresh] = true
	p.issued = append(p.issued, IssuedTokens{AccessToken: access, RefreshToken: refresh})
	p.handed = append(p.handed, access, refresh)
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, tokenBody(Ok{AccessToken: access, RefreshToken: refresh}))
}

// refreshGrant checks that the presented refresh token is one this provider issued and that is
// still current; only then a scripted step (if any) decides the answer.
func (p *OAuthProvider) refreshGrant(w http.ResponseWriter, r *http.Request, form url.Values) {
	presented := form.Get("refresh_token")
	p.mu.Lock()
	if !p.refreshes[presented] {
		p.mu.Unlock()
		invalidGrant(w)
		return
	}
	var step TokenStep
	if len(p.script) > 0 {
		step, p.script = p.script[0], p.script[1:]
	}
	if ok, isOk := step.(Ok); isOk && ok.RefreshToken != "" {
		delete(p.refreshes, presented) // rotated: the old one is no longer current
		p.refreshes[ok.RefreshToken] = true
	}
	lag := p.refreshLag
	p.mu.Unlock()
	if lag > 0 {
		timer := time.NewTimer(lag)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		case <-p.stopped:
			return
		}
	}
	switch s := step.(type) {
	case Error:
		status := s.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": s.Code})
	case Hang:
		select {
		case <-r.Context().Done():
		case <-p.stopped:
		}
	case Drop:
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case Ok:
		p.hand(writeToken(w, s))
	default:
		p.hand(writeToken(w, Ok{}))
	}
}

// writeToken sends a successful token response and returns the tokens it carried.
func writeToken(w http.ResponseWriter, o Ok) []string {
	body := tokenBody(o)
	writeJSON(w, http.StatusOK, body)
	var out []string
	for _, k := range []string{"access_token", "refresh_token"} {
		if v, ok := body[k].(string); ok {
			out = append(out, v)
		}
	}
	return out
}

func (p *OAuthProvider) hand(tokens []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handed = append(p.handed, tokens...)
}

func tokenBody(o Ok) map[string]any {
	if o.AccessToken == "" {
		o.AccessToken = randomToken("access-")
	}
	if o.TokenType == "" {
		o.TokenType = "bearer"
	}
	if o.ExpiresIn == 0 {
		o.ExpiresIn = 3600
	}
	body := map[string]any{"access_token": o.AccessToken, "token_type": o.TokenType}
	if o.ExpiresIn > 0 {
		body["expires_in"] = o.ExpiresIn
	}
	if o.RefreshToken != "" {
		body["refresh_token"] = o.RefreshToken
	}
	return body
}

func (p *OAuthProvider) revoke(w http.ResponseWriter, r *http.Request) {
	form, _ := url.ParseQuery(readBody(r))
	ok := p.clientOK(r)
	p.mu.Lock()
	p.attempts = append(p.attempts, RevokeAttempt{Token: form.Get("token"), Authorization: r.Header.Get("Authorization"), Accepted: ok})
	if ok {
		p.revoked = append(p.revoked, form.Get("token"))
	}
	p.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	w.WriteHeader(http.StatusOK)
}
