//go:build compat

package fakes

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// GroupsShape is how groups are written into the claims.
type GroupsShape int

const (
	// GroupsList is a JSON array of names.
	GroupsList GroupsShape = iota
	// GroupsObject is an object whose keys are the names, as Zitadel writes roles.
	GroupsObject
)

// ClaimsPlace says where the groups are emitted.
type ClaimsPlace int

const (
	// InIDToken puts the groups into the ID token (UserInfo has them too).
	InIDToken ClaimsPlace = iota
	// InUserInfo leaves the groups out of the ID token: only UserInfo has them.
	InUserInfo
)

// Claims is what the next login gets.
type Claims struct {
	Sub         string
	Name        string
	Email       string
	Groups      []string
	GroupsShape GroupsShape // list (default) or object
	Where       ClaimsPlace // ID token (default) or UserInfo only
	GroupsClaim string      // claim name of the groups; "groups" if empty
}

// DefaultClaims are used until NextLogin is called: a plain member.
var DefaultClaims = Claims{Sub: "fake-user", Name: "Fake User", Email: "fake-user@example.test", Groups: []string{"mcp-users"}}

// FakeIdP is an OpenID provider: discovery, JWKS, authorize (answers with a redirect carrying a
// code at once), token (ES256-signed ID tokens, PKCE S256, client_secret_post or Basic) and
// userinfo.
type FakeIdP struct { //nolint:revive // the name is fixed by the suite interface shared with the scenario tasks
	Issuer       string // http://host.docker.internal:<port>
	ClientID     string
	ClientSecret string

	mu         sync.Mutex
	key        *ecdsa.PrivateKey
	claims     Claims
	down       bool
	tokenCalls int
	grants     map[string]idpGrant
	access     map[string]Claims
	authorizes []url.Values
}

type idpGrant struct {
	claims      Claims
	nonce       string
	challenge   string
	redirectURI string
}

// StartIdP starts a provider that stops with the test.
func StartIdP(t *testing.T) *FakeIdP { //nolint:revive // the name is fixed by the suite interface shared with the scenario tasks
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("idp key: %v", err)
	}
	p := &FakeIdP{
		ClientID:     "mcp-gateway",
		ClientSecret: "client-secret",
		key:          key,
		claims:       DefaultClaims,
		grants:       map[string]idpGrant{},
		access:       map[string]Claims{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	p.Issuer = baseURL(serve(t, mux))
	return p
}

// OIDCSettings returns the values of OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET; it makes
// a FakeIdP usable as harness.Options.OIDC.
func (p *FakeIdP) OIDCSettings() (issuer, clientID, clientSecret string) {
	return p.Issuer, p.ClientID, p.ClientSecret
}

// NextLogin sets the claims of the logins that follow (until it is called again).
func (p *FakeIdP) NextLogin(c Claims) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = c
}

// Down makes discovery and the token endpoint answer 503 (and ends it with false).
func (p *FakeIdP) Down(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// TokenRequests returns how many requests the token endpoint received, including refused ones.
func (p *FakeIdP) TokenRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenCalls
}

// AuthorizeRequests returns the query of every authorize request, in order.
func (p *FakeIdP) AuthorizeRequests() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.authorizes...)
}

func (p *FakeIdP) isDown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down
}

func (p *FakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	if p.isDown() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         p.Issuer,
		"authorization_endpoint":                         p.Issuer + "/authorize",
		"token_endpoint":                                 p.Issuer + "/token",
		"jwks_uri":                                       p.Issuer + "/jwks",
		"userinfo_endpoint":                              p.Issuer + "/userinfo",
		"response_types_supported":                       []string{"code"},
		"subject_types_supported":                        []string{"public"},
		"id_token_signing_alg_values_supported":          []string{"ES256"},
		"token_endpoint_auth_methods_supported":          []string{"client_secret_post"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (p *FakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	point, err := p.key.PublicKey.Bytes() // 0x04 || X || Y, 32 bytes each
	if err != nil {
		http.Error(w, "key", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "k1",
		"x": b64(point[1:33]),
		"y": b64(point[33:]),
	}}})
}

// authorize is the browser step: the person is already signed in at the provider and consents at
// once, so the answer is the redirect back with a code.
func (p *FakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		redirect == "" || !hasScope(q.Get("scope"), "openid") {
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
	p.grants[code] = idpGrant{claims: p.claims, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectURI: redirect}
	p.mu.Unlock()
	rq := target.Query()
	rq.Set("code", code)
	if s := q.Get("state"); s != "" {
		rq.Set("state", s)
	}
	rq.Set("iss", p.Issuer)
	target.RawQuery = rq.Encode()
	// The fake exists to send the browser to whatever redirect_uri the application registered.
	http.Redirect(w, r, target.String(), http.StatusFound) //nolint:gosec // G710: see above
}

func (p *FakeIdP) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.tokenCalls++
	down := p.down
	p.mu.Unlock()
	if down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	id, secret, _ := clientCredentials(r)
	if id != p.ClientID || secret != p.ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	grant, ok := p.grants[code]
	delete(p.grants, code)
	p.mu.Unlock()
	if r.PostForm.Get("grant_type") != "authorization_code" || !ok || pkceChallenge(r.PostForm.Get("code_verifier")) != grant.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	access := randomToken("access-")
	p.mu.Lock()
	p.access[access] = grant.claims
	p.mu.Unlock()
	now := time.Now().Unix()
	payload := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "iat": now, "exp": now + 300, "nonce": grant.nonce}
	for k, v := range emitted(grant.claims, grant.claims.Where == InIDToken) {
		payload[k] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": p.sign(payload),
	})
}

func (p *FakeIdP) userinfo(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	c, ok := p.access[token]
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, emitted(c, true))
}

// emitted lists the claims of c; the groups only with withGroups, under the claim's name and shape.
func emitted(c Claims, withGroups bool) map[string]any {
	out := map[string]any{"sub": c.Sub}
	if c.Name != "" {
		out["name"] = c.Name
	}
	if c.Email != "" {
		out["email"] = c.Email
	}
	if withGroups && c.Groups != nil {
		name := c.GroupsClaim
		if name == "" {
			name = "groups"
		}
		if c.GroupsShape == GroupsObject {
			obj := make(map[string]any, len(c.Groups))
			for _, g := range c.Groups {
				obj[g] = map[string]string{}
			}
			out[name] = obj
		} else {
			out[name] = c.Groups
		}
	}
	return out
}

// sign makes an ES256 JWT (the signature is r||s, 32 bytes each, as JWS wants).
func (p *FakeIdP) sign(payload map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	signing := b64(head) + "." + b64(body)
	digest := sha256.Sum256([]byte(signing))
	rr, ss, err := ecdsa.Sign(rand.Reader, p.key, digest[:])
	if err != nil {
		panic(err) // signing with a valid key and the system random source does not fail
	}
	sig := append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64(sum[:])
}

func hasScope(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// clientCredentials reads the client ID and secret from HTTP Basic (form-decoded, RFC 6749
// section 2.3.1) or, without it, from the form (client_secret_post).
func clientCredentials(r *http.Request) (id, secret string, ok bool) {
	if h, found := strings.CutPrefix(r.Header.Get("Authorization"), "Basic "); found {
		return decodeBasic(h)
	}
	if v := r.PostForm.Get("client_secret"); v != "" || r.PostForm.Get("client_id") != "" {
		return r.PostForm.Get("client_id"), v, true
	}
	return "", "", false
}

func decodeBasic(encoded string) (id, secret string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	a, b, found := strings.Cut(string(raw), ":")
	if !found {
		return "", "", false
	}
	id, err1 := url.QueryUnescape(a)
	secret, err2 := url.QueryUnescape(b)
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	return id, secret, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
