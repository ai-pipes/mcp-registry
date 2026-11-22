package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

var ctx = context.Background()

// padded is a PAT as a person might paste it, with whitespace around.
const padded = "  perm:xyz  "

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func at(ms int64) time.Time     { return t0.Add(time.Duration(ms) * time.Millisecond) }
func tp(t time.Time) *time.Time { return &t }
func sp(s string) *string       { return &s }

func who(userID string, groups ...string) identity.Principal {
	if groups == nil {
		groups = []string{}
	}
	return identity.Principal{UserID: userID, Name: userID, Role: identity.RoleMember, Groups: groups, Via: identity.ViaSession}
}

var (
	alice = who("alice", "dev")
	bob   = who("bob", "dev")
)

// journal records the order of calls across all fake ports.
type journal struct {
	mu    sync.Mutex
	calls []string
}

func (j *journal) add(call string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, call)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.calls)
}

func (j *journal) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = nil
}

// lockMark tags the context WithLock hands to its callback: a fake port reached with such a
// context was used under the row lock.
type lockMark struct{}

// violations collects ports used under the lock.
type violations struct {
	mu   sync.Mutex
	list []string
}

func (v *violations) check(c context.Context, port string) {
	if c.Value(lockMark{}) == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.list = append(v.list, port)
}

func (v *violations) all() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.list)
}

func key(userID, server string) string { return userID + "\x00" + server }

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// fakeBox is reversible but never plain: every byte is XOR-ed, behind a marker. openFails lists
// plain texts whose sealed form this box can no longer open (a key mismatch for that value).
type fakeBox struct {
	mu        sync.Mutex
	openFails map[string]bool
	id        byte
	sealErr   error
}

func newBox(id byte) *fakeBox { return &fakeBox{openFails: map[string]bool{}, id: id} }

func (b *fakeBox) Seal(plain string) ([]byte, error) {
	if b.sealErr != nil {
		return nil, b.sealErr
	}
	out := []byte{0xee, b.id}
	for _, c := range []byte(plain) {
		out = append(out, c^0x5a)
	}
	return out, nil
}

func (b *fakeBox) Open(sealed []byte) (string, error) {
	if len(sealed) < 2 || sealed[0] != 0xee || sealed[1] != b.id {
		return "", errors.New("box: cannot open")
	}
	plain := make([]byte, 0, len(sealed))
	for _, c := range sealed[2:] {
		plain = append(plain, c^0x5a)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openFails[string(plain)] {
		return "", errors.New("box: cannot open")
	}
	return string(plain), nil
}

func (b *fakeBox) failOpenOf(plain string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openFails[plain] = true
}

func mustSeal(t testing.TB, b kernel.SecretBox, plain string) domain.Sealed {
	t.Helper()
	s, err := b.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Sealed(s)
}

// fakeStore keeps snapshots like a database: WithLock holds one mutex per key while the callback
// runs, and the callback receives a context tagged with lockMark.
type fakeStore struct {
	j   *journal
	v   *violations
	mu  sync.Mutex
	row map[string]domain.Snapshot
	// locks are the per-key mutexes.
	locks map[string]*sync.Mutex
	// beforeRead runs after the lock is taken and before the row is read.
	beforeRead func(userID, server string)
	failGet    error
	failLock   error
	// afterOp runs when a WithLock or Delete has finished (the request may be cancelled here).
	afterOp func()
}

func newStore(j *journal, v *violations) *fakeStore {
	return &fakeStore{j: j, v: v, row: map[string]domain.Snapshot{}, locks: map[string]*sync.Mutex{}}
}

func (s *fakeStore) put(c *domain.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row[key(c.UserID(), c.Server())] = c.Snapshot()
}

func (s *fakeStore) snap(userID, server string) (domain.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.row[key(userID, server)]
	return r, ok
}

func (s *fakeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.row)
}

func (s *fakeStore) Get(c context.Context, userID, server string) (*domain.Connection, error) {
	s.v.check(c, "store.get")
	s.j.add("store.get")
	if s.failGet != nil {
		return nil, s.failGet
	}
	if r, ok := s.snap(userID, server); ok {
		return domain.Restore(r), nil
	}
	return nil, nil
}

func (s *fakeStore) ListForUser(c context.Context, userID string) ([]*domain.Connection, error) {
	s.v.check(c, "store.list")
	s.j.add("store.list")
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.Connection
	for _, r := range s.row {
		if r.UserID == userID {
			out = append(out, domain.Restore(r))
		}
	}
	slices.SortFunc(out, func(a, b *domain.Connection) int { return strings.Compare(a.Server(), b.Server()) })
	return out, nil
}

func (s *fakeStore) Delete(c context.Context, userID, server string) (*domain.Connection, error) {
	s.v.check(c, "store.delete")
	s.j.add("store.delete")
	if s.afterOp != nil {
		defer s.afterOp()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.row[key(userID, server)]
	if !ok {
		return nil, nil
	}
	delete(s.row, key(userID, server))
	return domain.Restore(r), nil
}

func (s *fakeStore) ListKeys(c context.Context) ([]app.ConnectionKey, error) {
	s.v.check(c, "store.keys")
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.ConnectionKey
	for _, r := range s.row {
		out = append(out, app.ConnectionKey{UserID: r.UserID, Server: r.Server})
	}
	slices.SortFunc(out, func(a, b app.ConnectionKey) int {
		return strings.Compare(key(a.UserID, a.Server), key(b.UserID, b.Server))
	})
	return out, nil
}

func (s *fakeStore) WithLock(c context.Context, userID, server string, fn app.LockedFunc) error {
	if s.failLock != nil {
		return s.failLock
	}
	s.mu.Lock()
	m, ok := s.locks[key(userID, server)]
	if !ok {
		m = &sync.Mutex{}
		s.locks[key(userID, server)] = m
	}
	s.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	s.j.add("lock.begin")
	defer s.j.add("lock.end")
	if s.afterOp != nil {
		defer s.afterOp()
	}
	if s.beforeRead != nil {
		s.beforeRead(userID, server)
	}
	var current *domain.Connection
	if r, ok := s.snap(userID, server); ok {
		current = domain.Restore(r)
	}
	write, err := fn(context.WithValue(c, lockMark{}, true), current)
	if err != nil {
		return err
	}
	if write == nil {
		return nil
	}
	if write.UserID() != userID || write.Server() != server {
		return errors.New("fakeStore: the callback wrote another key")
	}
	s.j.add("lock.write")
	s.put(write)
	return nil
}

// fakePending keeps attempts; Consume deletes only the attempt of the asking person.
type fakePending struct {
	j  *journal
	v  *violations
	mu sync.Mutex
	m  map[string]domain.PendingConnect
}

func newPending(j *journal, v *violations) *fakePending {
	return &fakePending{j: j, v: v, m: map[string]domain.PendingConnect{}}
}

func (p *fakePending) Insert(c context.Context, pc domain.PendingConnect) error {
	p.v.check(c, "pending.insert")
	p.j.add("pending.insert")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[pc.State()] = pc
	return nil
}

func (p *fakePending) Consume(c context.Context, state, userID string) (*domain.PendingConnect, error) {
	p.v.check(c, "pending.consume")
	p.j.add("pending.consume")
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.m[state]
	if !ok || pc.UserID() != userID {
		return nil, nil
	}
	delete(p.m, state)
	return &pc, nil
}

func (p *fakePending) get(state string) (domain.PendingConnect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.m[state]
	return pc, ok
}

func (p *fakePending) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

func (p *fakePending) only() domain.PendingConnect {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pc := range p.m {
		return pc
	}
	panic("fakePending: no attempt")
}

// fakePkce makes recognisable tokens: odd calls are states, even calls verifiers.
type fakePkce struct {
	mu   sync.Mutex
	n    int
	fail error
}

func (p *fakePkce) RandomToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return "", p.fail
	}
	p.n++
	if p.n%2 == 1 {
		return fmt.Sprintf("STATEMARK-%d", p.n), nil
	}
	return fmt.Sprintf("VERIFIERMARK-%d", p.n), nil
}

func (p *fakePkce) Challenge(verifier string) string { return "S256(" + verifier + ")" }

// fakeAudit keeps the records.
type fakeAudit struct {
	j    *journal
	v    *violations
	mu   sync.Mutex
	list []kernel.AuditEntry
	err  error
	// ctxs are the contexts of the calls; cancelled counts calls made with an already-done one.
	ctxs      []context.Context
	cancelled int
}

func (a *fakeAudit) Record(c context.Context, e kernel.AuditEntry) error {
	a.v.check(c, "audit")
	a.j.add("audit:" + e.Action)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctxs = append(a.ctxs, c)
	if c.Err() != nil {
		a.cancelled++
	}
	if a.err != nil {
		return a.err
	}
	a.list = append(a.list, e)
	return nil
}

func (a *fakeAudit) all() []kernel.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.list)
}

func (a *fakeAudit) actions() []string {
	var out []string
	for _, e := range a.all() {
		out = append(out, e.Action)
	}
	return out
}

func (a *fakeAudit) expired() []kernel.AuditEntry {
	var out []kernel.AuditEntry
	for _, e := range a.all() {
		if e.Action == "connection.expired" {
			out = append(out, e)
		}
	}
	return out
}

// text renders every record as JSON, for the secrecy searches.
func (a *fakeAudit) text() string {
	var b strings.Builder
	for _, e := range a.all() {
		var actor, target string
		if e.ActorID != nil {
			actor = *e.ActorID
		}
		if e.Target != nil {
			target = *e.Target
		}
		meta, _ := json.Marshal(e.Meta)
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", actor, e.Action, target, meta)
	}
	return b.String()
}

// fakeOAuth is a scripted provider.
type fakeOAuth struct {
	j  *journal
	v  *violations
	mu sync.Mutex

	refreshCalls  []string
	exchangeCalls []exchangeCall
	revokeCalls   []string
	revokeSecrets []string

	// refreshFn, when set, answers refresh calls; otherwise `next` does.
	refreshFn func(c context.Context, refreshToken string) (app.TokenSet, error)
	next      app.TokenSet
	// exchangeErr makes the code exchange fail.
	exchangeErr error
	exchangeOut app.TokenSet
	revokeErr   error
	authErr     error
	// entered receives a value when a refresh call starts, gate blocks it until closed.
	entered chan string
	gate    chan struct{}
}

type exchangeCall struct {
	code, verifier, redirect, secret string
	secretSet                        bool
}

func newOAuth(j *journal, v *violations) *fakeOAuth {
	return &fakeOAuth{
		j: j, v: v,
		next:        app.TokenSet{AccessToken: "A-new", RefreshToken: "R-new"},
		exchangeOut: app.TokenSet{AccessToken: "A1", RefreshToken: "R1"},
	}
}

func (o *fakeOAuth) AuthorizeURL(_ catalog.OAuthClientConfig, in app.AuthorizeInput) (string, error) {
	if o.authErr != nil {
		return "", o.authErr
	}
	return "https://idp.example.com/authorize?state=" + in.State + "&code_challenge=" + in.CodeChallenge + "&redirect_uri=" + in.RedirectURI, nil
}

func (o *fakeOAuth) ExchangeCode(c context.Context, _ catalog.OAuthClientConfig, secret app.ClientSecret, in app.ExchangeInput) (app.TokenSet, error) {
	o.v.check(c, "oauth.exchange")
	o.j.add("oauth.exchange")
	sv, set := secret.Value()
	o.mu.Lock()
	o.exchangeCalls = append(o.exchangeCalls, exchangeCall{in.Code, in.CodeVerifier, in.RedirectURI, sv, set})
	err, out := o.exchangeErr, o.exchangeOut
	o.mu.Unlock()
	if err != nil {
		return app.TokenSet{}, err
	}
	return out, nil
}

func (o *fakeOAuth) Refresh(c context.Context, _ catalog.OAuthClientConfig, _ app.ClientSecret, refreshToken string) (app.TokenSet, error) {
	// The provider call is the one thing allowed under the lock: not a violation.
	o.j.add("oauth.refresh")
	o.mu.Lock()
	o.refreshCalls = append(o.refreshCalls, refreshToken)
	fn, next, entered, gate := o.refreshFn, o.next, o.entered, o.gate
	o.mu.Unlock()
	if entered != nil {
		entered <- refreshToken
	}
	if gate != nil {
		select {
		case <-gate:
		case <-c.Done():
			return app.TokenSet{}, c.Err()
		}
	}
	if fn != nil {
		return fn(c, refreshToken)
	}
	return next, nil
}

func (o *fakeOAuth) Revoke(c context.Context, _ catalog.OAuthClientConfig, secret app.ClientSecret, token string) error {
	o.v.check(c, "oauth.revoke")
	o.j.add("oauth.revoke")
	sv, _ := secret.Value()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.revokeCalls = append(o.revokeCalls, token)
	o.revokeSecrets = append(o.revokeSecrets, sv)
	return o.revokeErr
}

func (o *fakeOAuth) refreshed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.refreshCalls)
}

func (o *fakeOAuth) revoked() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.revokeCalls)
}

func (o *fakeOAuth) exchanged() []exchangeCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.exchangeCalls)
}

// fakeServer is a server as the fake catalog has it.
type fakeServer struct {
	slug, name   string
	kind         catalog.AuthKind
	groups       []string // who may use it; empty = everyone
	url          string
	oauth        *catalog.OAuthClientConfig
	clientSecret string
	hasSecret    bool
	disabled     bool
}

// fakeDirectory serves the servers; its methods are not allowed under the row lock.
type fakeDirectory struct {
	j       *journal
	v       *violations
	mu      sync.Mutex
	servers []*fakeServer
	// onConfig runs at the start of every Config call.
	onConfig  func()
	secretErr error
	listErr   error
	configErr error
}

func (d *fakeDirectory) find(slug string) *fakeServer {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.servers {
		if s.slug == slug && !s.disabled {
			return s
		}
	}
	return nil
}

func (d *fakeDirectory) configOf(s *fakeServer) app.ServerConfig {
	return app.ServerConfig{Slug: s.slug, URL: s.url, AuthKind: s.kind, OAuth: s.oauth}
}

func (d *fakeDirectory) PersonalServersFor(c context.Context, p identity.Principal) ([]app.PersonalServer, error) {
	d.v.check(c, "servers.personal")
	d.j.add("servers.personal")
	if d.listErr != nil {
		return nil, d.listErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []app.PersonalServer
	for _, s := range d.servers {
		if s.disabled {
			continue
		}
		if len(s.groups) > 0 && !slices.ContainsFunc(s.groups, func(g string) bool { return slices.Contains(p.Groups, g) }) {
			continue
		}
		if s.kind != catalog.AuthOAuthUser && s.kind != catalog.AuthPATUser {
			continue
		}
		out = append(out, app.PersonalServer{Slug: s.slug, Name: s.name, AuthKind: s.kind, Config: d.configOf(s)})
	}
	return out, nil
}

func (d *fakeDirectory) Config(c context.Context, slug string) (*app.ServerConfig, error) {
	d.v.check(c, "servers.config")
	d.j.add("servers.config")
	if d.onConfig != nil {
		d.onConfig()
	}
	if d.configErr != nil {
		return nil, d.configErr
	}
	s := d.find(slug)
	if s == nil {
		return nil, nil
	}
	cfg := d.configOf(s)
	return &cfg, nil
}

func (d *fakeDirectory) ClientSecret(c context.Context, slug string) (app.ClientSecret, error) {
	d.v.check(c, "servers.client_secret")
	d.j.add("servers.client_secret")
	if d.secretErr != nil {
		return app.ClientSecret{}, d.secretErr
	}
	s := d.find(slug)
	if s == nil || !s.hasSecret {
		return app.ClientSecret{}, nil
	}
	return app.NewClientSecret(s.clientSecret), nil
}

var giteaBase = "https://git.example.com/login/oauth/"

var giteaClient = catalog.OAuthClientConfig{
	AuthorizeURL: giteaBase + "authorize",
	TokenURL:     giteaBase + "exchange",
	ClientID:     "cid",
	Scopes:       []string{},
}

const giteaURL = "https://mcp.example.com/gitea"

// harness wires every fake.
type harness struct {
	t       *testing.T
	d       app.Deps
	j       *journal
	v       *violations
	clock   *fakeClock
	box     *fakeBox
	store   *fakeStore
	pending *fakePending
	oauth   *fakeOAuth
	dir     *fakeDirectory
	audit   *fakeAudit
	pkce    *fakePkce
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	j, v := &journal{}, &violations{}
	h := &harness{t: t, j: j, v: v, clock: &fakeClock{t: t0}, box: newBox(1)}
	h.store = newStore(j, v)
	h.pending = newPending(j, v)
	h.oauth = newOAuth(j, v)
	h.dir = &fakeDirectory{j: j, v: v}
	h.audit = &fakeAudit{j: j, v: v}
	h.pkce = &fakePkce{}
	oa := giteaClient
	h.dir.servers = []*fakeServer{
		{slug: "gitea", name: "Gitea", kind: catalog.AuthOAuthUser, url: giteaURL, oauth: &oa, clientSecret: "client-secret", hasSecret: true},
		{slug: "yt", name: "YouTrack", kind: catalog.AuthPATUser, url: "https://mcp.example.com/yt"},
		{slug: "secret", name: "Secret", kind: catalog.AuthOAuthUser, groups: []string{"ops"}, url: "https://mcp.example.com/secret", oauth: &oa, clientSecret: "client-secret", hasSecret: true},
		{slug: "shared", name: "Shared", kind: catalog.AuthSharedSecret, url: "https://mcp.example.com/shared"},
	}
	h.d = app.Deps{
		Connections: h.store, Pending: h.pending, Servers: h.dir, OAuth: h.oauth, Pkce: h.pkce,
		Box: h.box, Flights: app.NewSingleFlight[app.CredentialResult](), Clock: h.clock, Audit: h.audit,
		RedirectURI: "https://mcp.example.com/api/connections/callback",
	}
	return h
}

// replica is a second process over the same database: its own single flight.
func (h *harness) replica() app.Deps {
	d := h.d
	d.Flights = app.NewSingleFlight[app.CredentialResult]()
	return d
}

func (h *harness) gitea() *fakeServer { return h.dir.find("gitea") }

func (h *harness) fingerprintOf(s *fakeServer) string {
	cfg := h.dir.configOf(s)
	o := domain.ServerConfigOrigin{URL: cfg.URL, AuthKind: string(cfg.AuthKind)}
	if cfg.OAuth != nil {
		o.OAuth = &domain.OAuthEndpoints{AuthorizeURL: cfg.OAuth.AuthorizeURL, TokenURL: cfg.OAuth.TokenURL, ClientID: cfg.OAuth.ClientID}
	}
	return domain.ServerFingerprint(o)
}

// moveGitea points gitea's upstream URL elsewhere.
func (h *harness) moveGitea() { h.gitea().url = "https://evil.example.com/mcp" }

// withRevokeURL gives gitea's OAuth client a revoke URL (not part of the fingerprint).
func (h *harness) withRevokeURL() {
	c := *h.gitea().oauth
	c.RevokeURL = sp("https://git.example.com/revoke")
	h.gitea().oauth = &c
}

// seedOAuth stores an OAuth connection for user/gitea as if it had connected earlier.
func (h *harness) seedOAuth(expiresAt *time.Time, refresh, user string) {
	h.t.Helper()
	tokens := domain.SealedTokens{Access: mustSeal(h.t, h.box, "A1"), ExpiresAt: expiresAt}
	if refresh != "" {
		tokens.Refresh = mustSeal(h.t, h.box, refresh)
	}
	var revoke *string
	if o := h.gitea().oauth; o != nil {
		revoke = o.RevokeURL
	}
	h.store.put(domain.ConnectOAuth(domain.OAuthConnect{
		UserID: user, Server: "gitea", Tokens: tokens, IssuedFor: h.fingerprintOf(h.gitea()), RevokeURL: revoke, Now: t0,
	}))
}

// connectAlice runs start + callback the normal way.
func (h *harness) connect(p identity.Principal, server string) {
	h.t.Helper()
	url, err := app.StartConnect(ctx, h.d, app.StartConnectCommand{Principal: p, Server: server})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := app.CompleteConnect(ctx, h.d, app.CompleteConnectCommand{Principal: p, State: stateOf(url), Code: "the-code"}); err != nil {
		h.t.Fatal(err)
	}
}

func stateOf(url string) string {
	i := strings.Index(url, "state=")
	rest := url[i+len("state="):]
	if j := strings.Index(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// code names an outcome: "ok", the domain error code, or the error text.
func code(err error) string {
	if err == nil {
		return "ok"
	}
	if de, ok := kernel.AsDomainError(err); ok {
		return de.Code
	}
	return "error: " + err.Error()
}

func failureOf(r app.CredentialResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if r.Failure != "" {
		return string(r.Failure)
	}
	return "ok"
}

func (h *harness) resolve(user, server string) (app.CredentialResult, error) {
	return app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: user, Server: server})
}

func (h *harness) resolveForced(user, server string) (app.CredentialResult, error) {
	return app.ResolveCredential(ctx, h.d, app.ResolveQuery{UserID: user, Server: server, ForceRefresh: true})
}

func (h *harness) noLockViolations() {
	h.t.Helper()
	if v := h.v.all(); len(v) != 0 {
		h.t.Fatalf("ports used under the connection lock: %v", v)
	}
}
