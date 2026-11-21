package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

var ctx = context.Background()

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

// ServerWrite is one write of the fake server repository: the events that came with it and
// whether it wrote the secret (the real repository writes it only when the aggregate changed it).
type serverWrite struct {
	Op            string // "create" | "save"
	Slug          string
	ReadVersion   int64
	Events        []kernel.Event
	SecretWritten bool
}

// fakeServers keeps servers like the Postgres repository: Save writes only at the version the
// aggregate was read at, together with its events, and never touches the synchronisation state.
type fakeServers struct {
	j      *journal
	tools  *fakeTools
	outbox *[]kernel.Event

	mu     sync.Mutex
	rows   map[string]domain.ServerState
	writes []serverWrite
	// beforeSave runs inside Save before the version check (to let "someone else" write first).
	beforeSave func(slug string)
	// replaceHook runs at the start of ReplaceSealedSecret.
	replaceHook func(slug string)
	// failFind, if set, makes FindBySlug fail.
	failFind func(slug string) error
	// failSyncState, if set, makes SaveSyncState fail.
	failSyncState error
}

func state(s *domain.Server, version int64) domain.ServerState {
	return domain.ServerState{
		Slug: s.Slug(), Version: version, Name: s.Name(), Description: s.Description(), URL: s.URL(), Auth: s.Auth(),
		SealedSecret: slices.Clone(s.SealedSecret()), Access: s.Access(), TimeoutMs: s.TimeoutMs(), DailyLimit: s.DailyLimit(),
		Status: s.Status(), LastSyncAt: s.LastSyncAt(), LastSyncError: s.LastSyncError(), SyncUserID: s.SyncUserID(),
		CreatedAt: s.CreatedAt(), UpdatedAt: s.UpdatedAt(),
	}
}

func (r *fakeServers) FindBySlug(_ context.Context, slug string) (*domain.Server, error) {
	if r.failFind != nil {
		if err := r.failFind(slug); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok {
		return nil, nil
	}
	return domain.RehydrateServer(st), nil
}

func (r *fakeServers) List(context.Context) ([]app.ServerListing, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slugs := make([]string, 0, len(r.rows))
	for slug := range r.rows {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	out := make([]app.ServerListing, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, app.ServerListing{Server: domain.RehydrateServer(r.rows[slug]), ToolCount: r.tools.visibleCount(slug)})
	}
	return out, nil
}

func (r *fakeServers) Create(_ context.Context, s *domain.Server, events []kernel.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.rows[s.Slug()]; taken {
		return kernel.NewDomainError(domain.CodeServerExists, `server "`+s.Slug()+`" already exists`)
	}
	r.j.add("servers.create:" + s.Slug())
	r.rows[s.Slug()] = state(s, 0)
	*r.outbox = append(*r.outbox, events...)
	r.writes = append(r.writes, serverWrite{Op: "create", Slug: s.Slug(), Events: events, SecretWritten: s.SecretChanged()})
	return nil
}

func (r *fakeServers) Save(_ context.Context, s *domain.Server, events []kernel.Event) error {
	if r.beforeSave != nil {
		r.beforeSave(s.Slug())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.rows[s.Slug()]
	if !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `server "`+s.Slug()+`" not found`)
	}
	if stored.Version != s.Version() {
		return kernel.NewDomainError(domain.CodeServerConflict, `server "`+s.Slug()+`" was changed meanwhile; reload it and try again`)
	}
	r.j.add("servers.save:" + s.Slug())
	next := state(s, stored.Version+1)
	next.LastSyncAt, next.LastSyncError, next.SyncUserID = stored.LastSyncAt, stored.LastSyncError, stored.SyncUserID
	if !s.SecretChanged() {
		next.SealedSecret = stored.SealedSecret
	}
	r.rows[s.Slug()] = next
	*r.outbox = append(*r.outbox, events...)
	r.writes = append(r.writes, serverWrite{Op: "save", Slug: s.Slug(), ReadVersion: s.Version(), Events: events, SecretWritten: s.SecretChanged()})
	return nil
}

func (r *fakeServers) SaveSyncState(_ context.Context, slug string, rec domain.SyncRecord) error {
	if r.failSyncState != nil {
		return r.failSyncState
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `server "`+slug+`" not found`)
	}
	r.j.add("servers.syncstate:" + slug)
	s := domain.RehydrateServer(st)
	s.ApplySync(rec)
	st.LastSyncAt, st.LastSyncError, st.SyncUserID = s.LastSyncAt(), s.LastSyncError(), s.SyncUserID()
	r.rows[slug] = st
	return nil
}

func (r *fakeServers) ReplaceSealedSecret(_ context.Context, slug string, old, next []byte) (bool, error) {
	if r.replaceHook != nil {
		r.replaceHook(slug)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok || st.SealedSecret == nil || !bytes.Equal(st.SealedSecret, old) {
		return false, nil
	}
	st.SealedSecret = slices.Clone(next)
	r.rows[slug] = st
	return true, nil
}

// row is the stored state (a test reads it to check what was persisted).
func (r *fakeServers) row(t *testing.T, slug string) domain.ServerState {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[slug]
	if !ok {
		t.Fatalf("no stored server %q", slug)
	}
	return st
}

func (r *fakeServers) setRow(slug string, f func(*domain.ServerState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.rows[slug]
	f(&st)
	r.rows[slug] = st
}

type fakeTools struct {
	j      *journal
	outbox *[]kernel.Event
	exists func(slug string) bool

	mu     sync.Mutex
	rows   map[string]domain.ToolState
	writes []toolWrite
}

type toolWrite struct {
	Op     string // "save" | "apply"
	Server string
	Events []kernel.Event
}

func toolKey(server, name string) string { return server + "\x00" + name }

func toolState(t *domain.Tool) domain.ToolState {
	return domain.ToolState{ToolSnapshot: t.Snapshot(), Server: t.Server(), Hidden: t.Hidden(), RemovedAt: t.RemovedAt(), SyncedAt: t.SyncedAt()}
}

func (r *fakeTools) visibleCount(server string) int {
	n := 0
	for _, st := range r.rows {
		if st.Server == server && domain.RehydrateTool(st).Visible() {
			n++
		}
	}
	return n
}

func (r *fakeTools) Find(_ context.Context, server, name string) (*domain.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[toolKey(server, name)]
	if !ok {
		return nil, nil
	}
	return domain.RehydrateTool(st), nil
}

func (r *fakeTools) list(server string) []*domain.Tool {
	var out []*domain.Tool
	for _, st := range r.rows {
		if st.Server == server {
			out = append(out, domain.RehydrateTool(st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (r *fakeTools) ListByServer(_ context.Context, server string) ([]*domain.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.list(server), nil
}

func (r *fakeTools) Save(_ context.Context, t *domain.Tool, events []kernel.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[toolKey(t.Server(), t.Name())]; !ok {
		return kernel.NewDomainError(domain.CodeNotFound, `tool "`+t.Ref()+`" not found`)
	}
	r.j.add("tools.save:" + t.Ref())
	r.rows[toolKey(t.Server(), t.Name())] = toolState(t)
	*r.outbox = append(*r.outbox, events...)
	r.writes = append(r.writes, toolWrite{Op: "save", Server: t.Server(), Events: events})
	return nil
}

func (r *fakeTools) ApplySnapshot(_ context.Context, server string, snaps []domain.ToolSnapshot, now time.Time) (app.SnapshotResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exists != nil && !r.exists(server) {
		return app.SnapshotResult{}, kernel.NewDomainError(domain.CodeNotFound, `server "`+server+`" not found`)
	}
	rec := domain.ReconcileTools(server, r.list(server), snaps, now)
	r.j.add("tools.apply:" + server)
	for _, t := range rec.Upserts {
		r.rows[toolKey(t.Server(), t.Name())] = toolState(t)
	}
	*r.outbox = append(*r.outbox, rec.Events...)
	r.writes = append(r.writes, toolWrite{Op: "apply", Server: server, Events: rec.Events})
	return app.SnapshotResult{ChangedRefs: rec.ChangedRefs, AddedRefs: rec.AddedRefs, RemovedRefs: rec.RemovedRefs}, nil
}

func (r *fakeTools) row(server, name string) (domain.ToolState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.rows[toolKey(server, name)]
	return st, ok
}

// fakeBox is reversible and tagged by a key id; Open refuses another id.
type fakeBox struct{ id string }

func (b fakeBox) Seal(plain string) ([]byte, error) {
	return []byte(b.id + ":" + base64.StdEncoding.EncodeToString([]byte(plain))), nil
}

func (b fakeBox) Open(sealed []byte) (string, error) {
	prefix := b.id + ":"
	if !strings.HasPrefix(string(sealed), prefix) {
		return "", errors.New("wrong key")
	}
	plain, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(sealed), prefix))
	return string(plain), err
}

type fakeAudit struct {
	j       *journal
	mu      sync.Mutex
	entries []kernel.AuditEntry
}

func (a *fakeAudit) Record(_ context.Context, e kernel.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.j.add("audit:" + e.Action)
	a.entries = append(a.entries, e)
	return nil
}

func (a *fakeAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

func (a *fakeAudit) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = nil
}

// clock is a movable fixed clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

var t0 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)

// harness bundles the deps and the fakes behind them.
type harness struct {
	deps    app.Deps
	servers *fakeServers
	tools   *fakeTools
	audit   *fakeAudit
	clock   *clock
	journal *journal
	outbox  *[]kernel.Event
}

func newHarness() *harness {
	j := &journal{}
	outbox := &[]kernel.Event{}
	tools := &fakeTools{j: j, outbox: outbox, rows: map[string]domain.ToolState{}}
	servers := &fakeServers{j: j, tools: tools, outbox: outbox, rows: map[string]domain.ServerState{}}
	tools.exists = func(slug string) bool {
		servers.mu.Lock()
		defer servers.mu.Unlock()
		_, ok := servers.rows[slug]
		return ok
	}
	audit := &fakeAudit{j: j}
	clk := &clock{t: t0}
	return &harness{
		deps: app.Deps{
			Servers: servers, Tools: tools, Box: fakeBox{id: "k1"}, Clock: clk, Audit: audit,
			MaxResponseBytes: 1234,
		},
		servers: servers, tools: tools, audit: audit, clock: clk, journal: j, outbox: outbox,
	}
}

func (h *harness) eventTypes() []string {
	out := make([]string, 0, len(*h.outbox))
	for _, e := range *h.outbox {
		out = append(out, e.Type)
	}
	return out
}

// clean forgets the events and audit records written so far.
func (h *harness) clean() {
	*h.outbox = nil
	h.audit.reset()
	h.journal.reset()
	h.servers.mu.Lock()
	h.servers.writes = nil
	h.servers.mu.Unlock()
	h.tools.mu.Lock()
	h.tools.writes = nil
	h.tools.mu.Unlock()
}

var (
	admin  = identity.Principal{UserID: "admin-1", Role: identity.RoleAdmin, Groups: []string{}}
	admin2 = identity.Principal{UserID: "admin-2", Role: identity.RoleAdmin, Groups: []string{}}
)

func member(groups ...string) identity.Principal {
	if groups == nil {
		groups = []string{}
	}
	return identity.Principal{UserID: "u-1", Role: identity.RoleMember, Groups: groups}
}

func ptr[T any](v T) *T { return &v }

var (
	authNone   = domain.AuthMode{Kind: domain.AuthNone}
	authShared = domain.AuthMode{Kind: domain.AuthSharedSecret, Header: "X-API-Key", Template: "{secret}"}
	authPAT    = domain.AuthMode{Kind: domain.AuthPATUser, Header: "Authorization", Template: "Bearer {secret}"}
	authOAuth  = oauthMode()
	accessAll  = domain.Access{Kind: domain.AccessAll}
)

func groups(g ...string) domain.Access { return domain.Access{Kind: domain.AccessGroups, Groups: g} }

// fields is a valid set of editable fields; tests change what they test.
func fields(mut ...func(*app.ServerFields)) app.ServerFields {
	f := app.ServerFields{
		Name: "Gitea", Description: "Git hosting", URL: "https://mcp.example.com/gitea",
		Auth: authNone, Access: accessAll,
	}
	for _, m := range mut {
		m(&f)
	}
	return f
}

func input(slug string, mut ...func(*app.ServerFields)) app.ServerInput {
	return app.ServerInput{Slug: slug, Fields: fields(mut...)}
}

func createCmd(slug string, secret *string, mut ...func(*app.ServerFields)) app.CreateServerCommand {
	return app.CreateServerCommand{Principal: admin, Input: input(slug, mut...), Secret: secret}
}

func (h *harness) mustCreate(t *testing.T, slug string, secret *string, mut ...func(*app.ServerFields)) {
	t.Helper()
	if _, err := app.CreateServer(ctx, h.deps, createCmd(slug, secret, mut...)); err != nil {
		t.Fatalf("create %s: %v", slug, err)
	}
}

func snap(name string) domain.ToolSnapshot {
	return domain.ToolSnapshot{Name: name, Description: "does " + name, InputSchema: []byte(`{"type":"object"}`)}
}

func (h *harness) mustApply(t *testing.T, server string, snaps ...domain.ToolSnapshot) {
	t.Helper()
	if _, err := h.tools.ApplySnapshot(ctx, server, snaps, h.clock.Now()); err != nil {
		t.Fatalf("apply %s: %v", server, err)
	}
}

// code is the DomainError code of err ("ok" for nil).
func code(err error) string {
	if err == nil {
		return "ok"
	}
	de, ok := kernel.AsDomainError(err)
	if !ok {
		return "<not a DomainError: " + err.Error() + ">"
	}
	return de.Code
}

func wantCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := code(err); got != want {
		t.Fatalf("error = %v (code %q), want code %q", err, got, want)
	}
}

func wantError(t *testing.T, err error, wantCode, wantMessage string) {
	t.Helper()
	de, ok := kernel.AsDomainError(err)
	if !ok || de.Code != wantCode || de.Message != wantMessage {
		t.Fatalf("error = %v, want %s: %q", err, wantCode, wantMessage)
	}
}

func equalStrings(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// oauthMode builds the oauth_user mode field by field (a literal with "token" in a key trips gosec G101).
func oauthMode() domain.AuthMode {
	o := &domain.OAuthClientConfig{ClientID: "cid", Scopes: []string{}}
	o.AuthorizeURL = "https://git.example.com/authorize"
	o.TokenURL = "https://git.example.com/token"
	return domain.AuthMode{Kind: domain.AuthOAuthUser, Header: "Authorization", Template: "Bearer {secret}", OAuth: o}
}
