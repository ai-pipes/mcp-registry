// Package oidctest is an OpenID provider on httptest for tests: discovery, JWKS, token and
// UserInfo endpoints, ES256-signed ID tokens, and knobs for every way a provider can misbehave.
// It imports only the standard library and is imported only by _test files (the HTTP-layer tests
// of the identity module reuse it), so nothing here reaches the server binary.
package oidctest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Endpoint names a provider endpoint a fault can be attached to.
type Endpoint string

// The endpoints of the provider.
const (
	Discovery Endpoint = "discovery"
	JWKS      Endpoint = "jwks"
	Token     Endpoint = "token"
	UserInfo  Endpoint = "userinfo"
)

// Fault is how an endpoint misbehaves.
type Fault int

// The faults an endpoint can show.
const (
	FaultNone     Fault = iota
	Fault500            // answers 500
	FaultDrop           // closes the connection without an answer
	FaultHang           // never answers (until the provider stops)
	Fault400JSON        // answers 400 {"error":"invalid_grant"}
	FaultGate           // waits for ReleaseGate, then answers normally
	FaultRedirect       // answers 307 to the target given to SetRedirect
)

// GroupsShape is how groups are written into the claims.
type GroupsShape int

// The shapes groups can take.
const (
	GroupsList   GroupsShape = iota // a JSON array of names
	GroupsObject                    // an object whose keys are the names, as Zitadel writes roles
	GroupsNone                      // the claim is left out
)

// Place says where the groups are emitted.
type Place int

// The places groups can be emitted at.
const (
	InIDToken  Place = iota // in the ID token (UserInfo has them too)
	InUserInfo              // only UserInfo has them
)

// Claims is what the next login gets.
type Claims struct {
	Sub               string
	Name              string
	Email             string
	PreferredUsername string
	Groups            []string
	Shape             GroupsShape
	Where             Place
	GroupsClaim       string // "groups" if empty
	// IDTokenExtra and UserInfoExtra are written over the claims as they are (raw values).
	IDTokenExtra  map[string]any
	UserInfoExtra map[string]any
}

// Tamper makes the ID token wrong; the zero value is a correct token.
type Tamper struct {
	Issuer      string // iss, instead of the provider's
	Audience    string // aud, instead of the client ID
	Nonce       string // nonce, instead of the one the code was issued for
	Expired     bool   // exp in the past
	WrongKey    bool   // signed with a key the JWKS does not advertise
	OmitIDToken bool   // the token response has no id_token
	OmitNonce   bool   // the ID token has no nonce claim
	// Alg signs the ID token with "none" (no signature) or "HS256" (keyed with the client secret)
	// instead of ES256; the header says so.
	Alg string
	// Audiences, when set, is aud as an array (instead of the client ID alone); Azp is the azp claim.
	Audiences []string
	Azp       string
}

// Grant is what a code stands for; the test plays the browser step and issues the code itself.
type Grant struct {
	RedirectURI   string
	Nonce         string
	CodeChallenge string // S256 challenge of the verifier the test will send
	// Tamper, when set, replaces the provider's tamper settings for this code only.
	Tamper *Tamper
}

// Challenge is the S256 code challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64(sum[:])
}

// Provider is a running test identity provider.
type Provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string

	mu        sync.Mutex
	key       *ecdsa.PrivateKey
	otherKey  *ecdsa.PrivateKey
	claims    Claims
	tamper    Tamper
	faults    map[Endpoint]Fault
	requests  map[Endpoint]int
	forms     []url.Values
	grants    map[string]Grant
	access    map[string]Claims
	issParam  bool
	release   chan struct{}
	redirect  string
	metaIss   string
	algs      []string
	arrivals  chan Endpoint
	gate      chan struct{}
	gateOnce  sync.Once
	closeOnce sync.Once
	srv       *httptest.Server
}

// DefaultClaims is a plain member.
var DefaultClaims = Claims{Sub: "test-user", Name: "Test User", Email: "test-user@example.test", Groups: []string{"mcp-users"}}

// Start starts a provider that stops with the test. The issuer is http://127.0.0.1:<port>.
func Start(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{
		ClientID: "mcp-gateway", ClientSecret: "client-secret-value",
		claims: DefaultClaims, faults: map[Endpoint]Fault{}, requests: map[Endpoint]int{},
		grants: map[string]Grant{}, access: map[string]Claims{}, issParam: true,
		release: make(chan struct{}), algs: []string{"ES256"}, arrivals: make(chan Endpoint, 256), gate: make(chan struct{}),
	}
	var err error
	if p.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatalf("oidctest key: %v", err)
	}
	if p.otherKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatalf("oidctest key: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.guard(Discovery, p.discovery))
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("GET /jwks", p.guard(JWKS, p.jwks))
	mux.HandleFunc("POST /token", p.guard(Token, p.token))
	mux.HandleFunc("GET /userinfo", p.guard(UserInfo, p.userinfo))
	p.srv = httptest.NewServer(mux)
	p.Issuer = p.srv.URL
	t.Cleanup(p.Close)
	return p
}

// Close stops the provider; hanging requests are released first.
func (p *Provider) Close() {
	p.closeOnce.Do(func() {
		close(p.release)
		p.srv.CloseClientConnections()
		p.srv.Close()
	})
}

// SetClaims sets the claims of the logins that follow.
func (p *Provider) SetClaims(c Claims) { p.with(func() { p.claims = c }) }

// SetTamper sets how the ID tokens that follow are wrong.
func (p *Provider) SetTamper(t Tamper) { p.with(func() { p.tamper = t }) }

// SetFault makes an endpoint misbehave (FaultNone ends it).
func (p *Provider) SetFault(e Endpoint, f Fault) { p.with(func() { p.faults[e] = f }) }

// SetRedirect makes an endpoint answer 307 to target (FaultRedirect) until SetFault ends it.
func (p *Provider) SetRedirect(e Endpoint, target string) {
	p.with(func() { p.faults[e] = FaultRedirect; p.redirect = target })
}

// SetMetadataIssuer sets the issuer the provider announces (metadata, ID token iss, authorize
// redirect) when it differs from the address it listens on, e.g. by a trailing slash.
func (p *Provider) SetMetadataIssuer(iss string) { p.with(func() { p.metaIss = iss }) }

func (p *Provider) announced() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.metaIss != "" {
		return p.metaIss
	}
	return p.Issuer
}

// ReleaseGate lets the requests held by FaultGate through (and later ones pass at once).
func (p *Provider) ReleaseGate() { p.gateOnce.Do(func() { close(p.gate) }) }

// Drain forgets the arrivals recorded so far, so WaitFor sees only later requests.
func (p *Provider) Drain() {
	for {
		select {
		case <-p.arrivals:
		default:
			return
		}
	}
}

// SetAdvertisedAlgs sets id_token_signing_alg_values_supported (default ES256).
func (p *Provider) SetAdvertisedAlgs(algs ...string) { p.with(func() { p.algs = algs }) }

// WaitFor blocks until a request to the endpoint has arrived (requests that arrived before the
// call count too, once each) or the timeout passes.
func (p *Provider) WaitFor(e Endpoint, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case got := <-p.arrivals:
			if got == e {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// SetIssParamSupported sets authorization_response_iss_parameter_supported in the metadata.
func (p *Provider) SetIssParamSupported(v bool) { p.with(func() { p.issParam = v }) }

// Requests is how many requests an endpoint received, faulty ones included.
func (p *Provider) Requests(e Endpoint) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[e]
}

// TokenForms are the bodies of the token requests, in order.
func (p *Provider) TokenForms() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.forms...)
}

// IssueCode registers an authorization code for a grant and returns it.
func (p *Provider) IssueCode(g Grant) string {
	code := random("code-")
	p.with(func() { p.grants[code] = g })
	return code
}

func (p *Provider) with(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

// guard counts the request and applies the endpoint's fault.
func (p *Provider) guard(e Endpoint, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests[e]++
		f, target := p.faults[e], p.redirect
		p.mu.Unlock()
		select {
		case p.arrivals <- e:
		default:
		}
		switch f {
		case FaultGate:
			select {
			case <-p.gate:
				h(w, r)
			case <-p.release:
			case <-r.Context().Done():
			}
		case FaultRedirect:
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		case Fault500:
			http.Error(w, "boom", http.StatusInternalServerError)
		case FaultDrop:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case FaultHang:
			select {
			case <-p.release:
			case <-r.Context().Done():
			}
		case Fault400JSON:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		default:
			h(w, r)
		}
	}
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	iss, algs := p.issParam, p.algs
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         p.announced(),
		"authorization_endpoint":                         p.Issuer + "/authorize",
		"token_endpoint":                                 p.Issuer + "/token",
		"jwks_uri":                                       p.Issuer + "/jwks",
		"userinfo_endpoint":                              p.Issuer + "/userinfo",
		"response_types_supported":                       []string{"code"},
		"subject_types_supported":                        []string{"public"},
		"id_token_signing_alg_values_supported":          algs,
		"token_endpoint_auth_methods_supported":          []string{"client_secret_post"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": iss,
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	point, err := p.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		http.Error(w, "key", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "k1",
		"x": b64(point[1:33]), "y": b64(point[33:]),
	}}})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	form := r.PostForm
	p.mu.Lock()
	p.forms = append(p.forms, form)
	grant, ok := p.grants[form.Get("code")]
	delete(p.grants, form.Get("code"))
	claims, tamper := p.claims, p.tamper
	if ok && grant.Tamper != nil {
		tamper = *grant.Tamper
	}
	p.mu.Unlock()
	// client_secret_post only: credentials in the form, never in a header.
	if r.Header.Get("Authorization") != "" || form.Get("client_id") != p.ClientID || form.Get("client_secret") != p.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if form.Get("grant_type") != "authorization_code" || !ok ||
		Challenge(form.Get("code_verifier")) != grant.CodeChallenge ||
		form.Get("redirect_uri") != grant.RedirectURI {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	access := random("access-")
	p.with(func() { p.access[access] = claims })
	now := time.Now()
	payload := map[string]any{
		"iss": orDefault(tamper.Issuer, p.announced()), "aud": orDefault(tamper.Audience, p.ClientID),
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": orDefault(tamper.Nonce, grant.Nonce),
	}
	if len(tamper.Audiences) > 0 {
		payload["aud"] = tamper.Audiences
	}
	if tamper.Azp != "" {
		payload["azp"] = tamper.Azp
	}
	if tamper.OmitNonce {
		delete(payload, "nonce")
	}
	if tamper.Expired {
		payload["exp"] = now.Add(-time.Hour).Unix()
		payload["iat"] = now.Add(-2 * time.Hour).Unix()
	}
	for k, v := range emitted(claims, claims.Where == InIDToken, claims.IDTokenExtra) {
		payload[k] = v
	}
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300}
	if !tamper.OmitIDToken {
		key := p.key
		if tamper.WrongKey {
			key = p.otherKey
		}
		switch tamper.Alg {
		case "none":
			resp["id_token"] = signUnsigned(payload)
		case "HS256":
			resp["id_token"] = signHMAC([]byte(p.ClientSecret), payload)
		default:
			resp["id_token"] = sign(key, payload)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	p.mu.Lock()
	claims, ok := Claims{}, false
	if len(h) > len(prefix) {
		claims, ok = p.access[h[len(prefix):]]
	}
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, emitted(claims, true, claims.UserInfoExtra))
}

func emitted(c Claims, withGroups bool, extra map[string]any) map[string]any {
	out := map[string]any{"sub": c.Sub}
	if c.Name != "" {
		out["name"] = c.Name
	}
	if c.Email != "" {
		out["email"] = c.Email
	}
	if c.PreferredUsername != "" {
		out["preferred_username"] = c.PreferredUsername
	}
	if withGroups && c.Shape != GroupsNone {
		name := orDefault(c.GroupsClaim, "groups")
		if c.Shape == GroupsObject {
			obj := make(map[string]any, len(c.Groups))
			for _, g := range c.Groups {
				obj[g] = map[string]string{}
			}
			out[name] = obj
		} else {
			out[name] = append([]string{}, c.Groups...)
		}
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// sign makes an ES256 JWT (r||s, 32 bytes each, as JWS wants).
func sign(key *ecdsa.PrivateKey, payload map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	signing := b64(head) + "." + b64(body)
	digest := sha256.Sum256([]byte(signing))
	rr, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		panic(err) // a valid key and the system random source do not fail
	}
	sig := append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	return signing + "." + b64(sig)
}

func signUnsigned(payload map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	return b64(head) + "." + b64(body) + "."
}

func signHMAC(secret []byte, payload map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "HS256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	signing := b64(head) + "." + b64(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

// authorize is the browser step: the person consents at once and the answer is the redirect back
// with a code, the state and iss (RFC 9207).
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || redirect == "" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid") {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	code := p.IssueCode(Grant{RedirectURI: redirect, Nonce: q.Get("nonce"), CodeChallenge: q.Get("code_challenge")})
	rq := target.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	rq.Set("iss", p.announced())
	target.RawQuery = rq.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound) //nolint:gosec // G710: a test provider sends the browser where the client registered
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func random(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + b64(b)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
