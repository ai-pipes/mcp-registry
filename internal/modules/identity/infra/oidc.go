package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ai-pipes/mcp-registry/internal/modules/identity/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/logging"
)

// Defaults of the configuration (the TypeScript DEFAULT_OIDC_SCOPES and DEFAULT_GROUPS_CLAIM).
const (
	DefaultOIDCScopes  = "openid profile email groups"
	DefaultGroupsClaim = "groups"
)

const (
	// discoveryTimeout gives up on a dead provider, so a login attempt never hangs on it.
	discoveryTimeout = 5 * time.Second
	// requestTimeout bounds every other request to the provider (token, JWKS, UserInfo). The
	// TypeScript library's own default is unknown (UNVERIFIED); 10 s is our choice.
	requestTimeout = 10 * time.Second
)

// OIDCConfig is what the provider adapter needs; the composition fills it from the configuration.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURI is the absolute URL of /api/auth/callback on this server.
	RedirectURI string
	// Scopes is space-separated; empty means DefaultOIDCScopes.
	Scopes string
	// GroupsClaim is the claim holding the groups; empty means DefaultGroupsClaim.
	GroupsClaim string
	Logger      *slog.Logger
	// HTTPClient is optional (tests); its Transport, Jar and CheckRedirect are used, and a
	// request timeout of requestTimeout is applied when it has none.
	HTTPClient *http.Client
}

// OIDCProvider is any OIDC provider through go-oidc and x/oauth2: Authorization Code + PKCE,
// client_secret_post, ID token signature verified. Discovery is lazy: the server starts without
// the provider; the metadata is fetched on first use (5 s limit, concurrent callers share one
// request), cached once it succeeded and fetched again on the next use after a failure.
type OIDCProvider struct {
	cfg    OIDCConfig
	log    *slog.Logger
	client *http.Client
	// outages counts what looks like a provider outage on the wire (no answer, or a 5xx), so a
	// failure inside go-oidc, which hides its causes in text, can be classified.
	outages atomic.Int64

	mu     sync.Mutex
	found  *discovered
	flight *discoveryFlight
}

// NewOIDCProvider builds the adapter; it makes no request.
func NewOIDCProvider(cfg OIDCConfig) *OIDCProvider {
	if cfg.Scopes == "" {
		cfg.Scopes = DefaultOIDCScopes
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = DefaultGroupsClaim
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &OIDCProvider{cfg: cfg, log: log}
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	timeout := base.Timeout
	if timeout == 0 {
		timeout = requestTimeout
	}
	p.client = &http.Client{
		Transport:     &watchTransport{next: rt, outages: &p.outages},
		CheckRedirect: base.CheckRedirect, Jar: base.Jar, Timeout: timeout,
	}
	return p
}

type discovered struct {
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	issuer   string
	// issParam is authorization_response_iss_parameter_supported (RFC 9207).
	issParam bool
}

type discoveryFlight struct {
	done chan struct{}
	res  *discovered
	err  error
}

func errUnavailable() error {
	return domain.Err(domain.CodeIDPUnavailable, "the identity provider is unavailable")
}

// discovery returns the provider metadata, fetching it when there is none.
func (p *OIDCProvider) discovery(ctx context.Context) (*discovered, error) {
	if ctx.Err() != nil {
		return nil, errUnavailable()
	}
	p.mu.Lock()
	if p.found != nil {
		d := p.found
		p.mu.Unlock()
		return d, nil
	}
	f := p.flight
	if f == nil {
		f = &discoveryFlight{done: make(chan struct{})}
		p.flight = f
		// The flight outlives the caller that started it: other callers wait on it too.
		go p.discover(f) //nolint:gosec // G118: the shared flight must outlive the caller that started it; discover bounds itself with discoveryTimeout
	}
	p.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, errUnavailable()
	}
	if f.err != nil {
		return nil, errUnavailable()
	}
	return f.res, nil
}

func (p *OIDCProvider) discover(f *discoveryFlight) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	d, err := p.fetchDiscovery(ctx)
	p.mu.Lock()
	if err == nil {
		p.found = d
	}
	p.flight = nil
	p.mu.Unlock()
	if err != nil {
		// Logged before the callers are released, so a caller that has its answer sees the record.
		p.log.Warn("oidc discovery failed", logging.ErrAttr(err))
	}
	f.res, f.err = d, err
	close(f.done)
}

func (p *OIDCProvider) fetchDiscovery(ctx context.Context) (*discovered, error) {
	prov, err := oidc.NewProvider(oidc.ClientContext(ctx, p.client), p.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	var meta struct {
		IssParam bool `json:"authorization_response_iss_parameter_supported"`
	}
	if err := prov.Claims(&meta); err != nil {
		return nil, err
	}
	endpoint := prov.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInParams // client_secret_post, one request, no probing
	return &discovered{
		provider: prov,
		oauth: oauth2.Config{
			ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret, Endpoint: endpoint,
			RedirectURL: p.cfg.RedirectURI, Scopes: strings.Fields(p.cfg.Scopes),
		},
		// Provider.Verifier takes the signing algorithms from id_token_signing_alg_values_supported.
		verifier: prov.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}),
		issuer:   p.cfg.Issuer,
		issParam: meta.IssParam,
	}, nil
}

// AuthorizationURL implements app.OIDCProvider.
func (p *OIDCProvider) AuthorizationURL(ctx context.Context, in app.OIDCAuthorizationInput) (*url.URL, error) {
	d, err := p.discovery(ctx)
	if err != nil {
		return nil, err
	}
	raw := d.oauth.AuthCodeURL(in.State,
		oidc.Nonce(in.Nonce),
		oauth2.SetAuthURLParam("code_challenge", in.CodeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	return url.Parse(raw)
}

// Exchange implements app.OIDCProvider.
func (p *OIDCProvider) Exchange(ctx context.Context, in app.OIDCExchangeInput) (domain.OIDCClaims, error) {
	d, err := p.discovery(ctx)
	if err != nil {
		return domain.OIDCClaims{}, err
	}
	// RFC 9207: an iss parameter must be the issuer; a provider that announces the parameter
	// must send it. Checked before the code is spent.
	if in.Iss != nil && *in.Iss != d.issuer {
		return domain.OIDCClaims{}, errors.New("oidc: the iss parameter is not the issuer")
	}
	if in.Iss == nil && d.issParam {
		return domain.OIDCClaims{}, errors.New("oidc: the provider announces the iss parameter and the callback has none")
	}

	cctx := oidc.ClientContext(ctx, p.client)
	tctx, cancel := context.WithTimeout(cctx, requestTimeout)
	tok, err := d.oauth.Exchange(tctx, in.Code, oauth2.VerifierOption(in.CodeVerifier))
	cancel()
	if err != nil {
		return domain.OIDCClaims{}, p.tokenError(ctx, err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return domain.OIDCClaims{}, errors.New("oidc: no ID token")
	}

	before := p.outages.Load()
	vctx, cancel := context.WithTimeout(cctx, requestTimeout)
	idt, err := d.verifier.Verify(vctx, rawID)
	cancel()
	if err != nil {
		if p.outages.Load() > before && ctx.Err() == nil {
			p.log.Warn("oidc key set unavailable", logging.ErrAttr(err))
			return domain.OIDCClaims{}, errUnavailable()
		}
		return domain.OIDCClaims{}, fmt.Errorf("oidc: ID token rejected: %w", err)
	}
	if idt.Nonce != in.Nonce {
		return domain.OIDCClaims{}, errors.New("oidc: the ID token's nonce is not ours")
	}
	if idt.Subject == "" {
		return domain.OIDCClaims{}, errors.New("oidc: the ID token has no subject")
	}
	var id map[string]json.RawMessage
	if err := idt.Claims(&id); err != nil {
		return domain.OIDCClaims{}, errors.New("oidc: the ID token claims cannot be read")
	}

	claim := p.cfg.GroupsClaim
	profile := id
	groups, ok := groupsOf(id[claim])
	if !ok {
		// Some providers keep groups out of the ID token; ask UserInfo as a fallback.
		info, err := p.userInfo(cctx, d, tok, idt.Subject)
		if err != nil {
			// No usable UserInfo: the person simply has no groups, so the policy refuses them.
			// Logged so that a provider outage is not mistaken for a missing group.
			p.log.Warn("oidc userinfo failed; treating the person as having no groups",
				slog.String("sub", idt.Subject), logging.ErrAttr(err))
		} else {
			profile = info
			if groups, ok = groupsOf(info[claim]); !ok {
				p.log.Warn("oidc userinfo has no usable groups claim; treating the person as having no groups",
					slog.String("sub", idt.Subject))
			}
		}
	}
	if !ok {
		groups = []string{}
	}
	email := text(profile["email"])
	if email == nil {
		email = text(id["email"])
	}
	name := idt.Subject
	switch {
	case text(profile["name"]) != nil:
		name = *text(profile["name"])
	case text(profile["preferred_username"]) != nil:
		name = *text(profile["preferred_username"])
	case email != nil:
		name = *email
	}
	return domain.OIDCClaims{Sub: idt.Subject, Name: name, Email: email, Groups: groups}, nil
}

// userInfo reads the UserInfo claims; the answer must be about the same person.
func (p *OIDCProvider) userInfo(ctx context.Context, d *discovered, tok *oauth2.Token, sub string) (map[string]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	info, err := d.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return nil, err
	}
	if info.Subject != sub {
		return nil, errors.New("oidc: UserInfo is about another subject")
	}
	var claims map[string]json.RawMessage
	if err := info.Claims(&claims); err != nil {
		return nil, errors.New("oidc: UserInfo claims cannot be read")
	}
	return claims, nil
}

// tokenError maps a failed code exchange: no answer, a timeout or a 5xx is an outage; anything
// else (an OAuth error, a 4xx, an unreadable answer) is the provider refusing. The refusal never
// carries the response body, which may quote what we sent.
func (p *OIDCProvider) tokenError(ctx context.Context, err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		if status >= http.StatusInternalServerError {
			p.log.Warn("oidc token endpoint unavailable", logging.ErrAttr(err))
			return errUnavailable()
		}
		return fmt.Errorf("oidc: the token endpoint refused the code (HTTP %d, %q)", status, re.ErrorCode)
	}
	var ue *url.Error
	if errors.As(err, &ue) && !errors.Is(ctx.Err(), context.Canceled) {
		p.log.Warn("oidc token endpoint unavailable", logging.ErrAttr(err))
		return errUnavailable()
	}
	return errors.New("oidc: the token response is unusable")
}

// groupsOf reads group names from a claim: a JSON array of strings (most providers; all elements
// must be strings), or an object whose keys are the names (Zitadel's roles), returned sorted.
// Anything else, or an absent claim, is not a list of groups.
func groupsOf(raw json.RawMessage) ([]string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	switch raw[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			var s string
			if err := json.Unmarshal(it, &s); err != nil || bytes.TrimSpace(it)[0] != '"' {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, false
		}
		out := make([]string, 0, len(obj))
		for k := range obj {
			out = append(out, k)
		}
		sort.Strings(out)
		return out, true
	}
	return nil, false
}

// text is a claim's value when it is a non-empty string.
func text(raw json.RawMessage) *string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == "" {
		return nil
	}
	return &s
}

// watchTransport counts requests that got no answer or a 5xx.
type watchTransport struct {
	next    http.RoundTripper
	outages *atomic.Int64
}

func (w *watchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(r)
	if err != nil || resp.StatusCode >= http.StatusInternalServerError {
		w.outages.Add(1)
	}
	return resp, err
}
