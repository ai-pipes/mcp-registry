package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/connections"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
	"github.com/ai-pipes/mcp-registry/internal/modules/identity"
)

type fakeCatalog struct {
	mu        sync.Mutex
	tools     map[string]catalog.ToolView
	servers   map[string]*catalog.ServerCallTarget
	secrets   map[string]string
	toolErr   error
	serverErr error
	secretErr error
	// secretLookup, when set, is answered by ServerSecret without a secret.
	secretLookup catalog.SecretLookup
	onServerFor  func()
	// onSecret runs before the secret is read (an admin editing the server between the reads).
	onSecret   func()
	toolCtxErr error // ctx.Err() as ToolFor saw it on its last call
}

func (c *fakeCatalog) ToolFor(ctx context.Context, _ app.Principal, ref string) (catalog.ToolView, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolCtxErr = ctx.Err()
	if c.toolErr != nil {
		return catalog.ToolView{}, false, c.toolErr
	}
	t, ok := c.tools[ref]
	return t, ok, nil
}

func (c *fakeCatalog) ServerFor(_ context.Context, slug string) (*catalog.ServerCallTarget, error) {
	if c.onServerFor != nil {
		c.onServerFor()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.serverErr != nil {
		return nil, c.serverErr
	}
	return c.servers[slug], nil
}

// ServerSecret models one read of the stored row: the secret is handed out only while the row's
// destination (auth kind, and the URL for shared_secret) is the target's.
func (c *fakeCatalog) ServerSecret(_ context.Context, target catalog.ServerCallTarget) (string, catalog.SecretLookup, error) {
	if c.onSecret != nil {
		c.onSecret()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secretErr != nil {
		return "", "", c.secretErr
	}
	if c.secretLookup != "" {
		return "", c.secretLookup, nil
	}
	row := c.servers[target.Slug]
	if row == nil || row.Auth.Kind != target.Auth.Kind || (row.Auth.Kind == catalog.AuthSharedSecret && row.URL != target.URL) {
		return "", catalog.SecretMoved, nil
	}
	s, ok := c.secrets[target.Slug]
	if !ok {
		return "", catalog.SecretNotSet, nil
	}
	return s, catalog.SecretFound, nil
}

type resolveCall struct {
	userID string
	target catalog.ServerCallTarget
	force  bool
}

type fakeCreds struct {
	mu         sync.Mutex
	results    []connections.CredentialResult // consumed in order, the last one repeats
	err        error
	calls      []resolveCall
	expired    []connections.MarkExpiredCommand
	expiredErr error
}

func (f *fakeCreds) ResolveForSending(_ context.Context, userID string, target catalog.ServerCallTarget, force bool) (connections.CredentialResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, resolveCall{userID, target, force})
	if f.err != nil {
		return connections.CredentialResult{}, f.err
	}
	i := min(len(f.calls)-1, len(f.results)-1)
	return f.results[i], nil
}

func (f *fakeCreds) MarkExpired(_ context.Context, c connections.MarkExpiredCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, c)
	return f.expiredErr
}

type fakeValidator struct {
	mu    sync.Mutex
	bad   *app.ArgsCheck
	seen  []string
	schem []string
	// panicWith, when set, is the value Validate panics with.
	panicWith string
}

func (v *fakeValidator) Validate(ref string, schema, args json.RawMessage) app.ArgsCheck {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, string(args))
	v.schem = append(v.schem, ref+"|"+string(schema))
	if v.panicWith != "" {
		panic(v.panicWith)
	}
	if v.bad != nil {
		return *v.bad
	}
	return app.ArgsCheck{OK: true}
}

type fakeLog struct {
	mu      sync.Mutex
	entries []app.CallLogEntry
	stored  []app.CallRecord
	err     error
	filter  app.CallLogFilter
	listed  bool
	cutoff  time.Time
	delErr  error
	ctxErr  error // ctx.Err() seen by the last Record
}

func (l *fakeLog) Record(ctx context.Context, e app.CallLogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ctxErr = ctx.Err()
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, e)
	return nil
}

func (l *fakeLog) List(_ context.Context, f app.CallLogFilter) ([]app.CallRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listed, l.filter = true, f
	return l.stored, nil
}

func (l *fakeLog) DeleteOlderThan(_ context.Context, t time.Time) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cutoff = t
	return 4, l.delErr
}

type fakeQuota struct {
	mu       sync.Mutex
	counts   map[string]int
	requests []app.QuotaRequest
	err      error
	before   string
	delCalls int
}

func (q *fakeQuota) TryConsume(_ context.Context, r app.QuotaRequest) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requests = append(q.requests, r)
	if q.err != nil {
		return false, q.err
	}
	if q.counts == nil {
		q.counts = map[string]int{}
	}
	k := r.UserID + "|" + r.Server + "|" + r.Day
	if q.counts[k] >= r.Limit {
		return false, nil
	}
	q.counts[k]++
	return true, nil
}

func (q *fakeQuota) DeleteDaysBefore(_ context.Context, day string) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.before = day
	q.delCalls++
	return 2, nil
}

// upstreamStep is what the fake upstream answers to one call.
type upstreamStep struct {
	result json.RawMessage
	err    error
	wait   func(ctx context.Context) error // runs first; its error is the answer
}

type upstreamCall struct {
	target kernel.UpstreamTarget
	name   string
	args   string
	secret string
}

type fakeUpstream struct {
	mu    sync.Mutex
	steps []upstreamStep
	calls []upstreamCall
}

func (u *fakeUpstream) ListTools(context.Context, kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	return nil, errors.New("not used")
}

func (u *fakeUpstream) CallTool(ctx context.Context, target kernel.UpstreamTarget, name string, args json.RawMessage) (kernel.UpstreamCallResult, error) {
	u.mu.Lock()
	i := len(u.calls)
	u.calls = append(u.calls, upstreamCall{target: target, name: name, args: string(args)})
	var step upstreamStep
	if len(u.steps) > 0 {
		step = u.steps[min(i, len(u.steps)-1)]
	} else {
		step = upstreamStep{result: json.RawMessage(`{"content":[]}`)}
	}
	u.mu.Unlock()
	if step.wait != nil {
		if err := step.wait(ctx); err != nil {
			return nil, err
		}
	}
	return step.result, step.err
}

// equalSecrets compares plainly and counts its calls.
type equalSecrets struct {
	mu    sync.Mutex
	calls int
}

func (e *equalSecrets) Equal(a, b string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return a == b
}

func (e *equalSecrets) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type logRecord struct {
	msg  string
	args []any
}

type fakeLogger struct {
	mu      sync.Mutex
	records []logRecord
}

func (l *fakeLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, logRecord{msg, args})
}

func (l *fakeLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprint(l.records)
}

// stepClock moves by the given steps per Now call (the last step repeats).
type stepClock struct {
	mu    sync.Mutex
	t     time.Time
	steps []time.Duration
	n     int
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	if c.n < len(c.steps) {
		c.t = c.t.Add(c.steps[c.n])
	} else if len(c.steps) > 0 {
		c.t = c.t.Add(c.steps[len(c.steps)-1])
	}
	c.n++
	return t
}

type harness struct {
	cat   *fakeCatalog
	creds *fakeCreds
	val   *fakeValidator
	log   *fakeLog
	quota *fakeQuota
	up    *fakeUpstream
	lg    *fakeLogger
	sec   *equalSecrets
	clock kernel.Clock
	d     app.Deps
	alice app.Principal
}

var epoch = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func newHarness() *harness {
	h := &harness{
		cat:   &fakeCatalog{tools: map[string]catalog.ToolView{}, servers: map[string]*catalog.ServerCallTarget{}, secrets: map[string]string{}},
		creds: &fakeCreds{results: []connections.CredentialResult{{Secret: "tok-1"}}},
		val:   &fakeValidator{},
		log:   &fakeLog{},
		quota: &fakeQuota{},
		up:    &fakeUpstream{},
		lg:    &fakeLogger{},
		sec:   &equalSecrets{},
		clock: kernel.FixedClock{T: epoch},
		alice: identity.Principal{UserID: "u-alice", Role: identity.RoleMember, Via: identity.ViaToken, TokenID: "t-1"},
	}
	h.addServer("gitea", "Gitea", catalog.AuthMode{Kind: catalog.AuthNone}, nil)
	h.addTool("gitea", "create_issue")
	h.rebuild()
	return h
}

func (h *harness) rebuild() {
	h.d = app.Deps{
		Catalog: h.cat, Credentials: h.creds, Validator: h.val, CallLog: h.log, Quota: h.quota,
		Upstream: h.up, Clock: h.clock, Logger: h.lg, Secrets: h.sec,
		PublicURL: "https://mcp.example.com/", MaxResponseBytes: 20 << 20,
	}
}

func (h *harness) addServer(slug, name string, auth catalog.AuthMode, limit *int) *catalog.ServerCallTarget {
	s := &catalog.ServerCallTarget{Slug: slug, Name: name, URL: "https://" + slug + ".example.com/mcp", Auth: auth, TimeoutMs: 30000, DailyLimit: limit}
	h.cat.servers[slug] = s
	return s
}

func (h *harness) addTool(server, name string) {
	ref := server + "." + name
	h.cat.tools[ref] = catalog.ToolView{Server: server, Name: name, Ref: ref, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (h *harness) call(ctx context.Context, ref, args string) (domain.ToolResult, error) {
	var raw json.RawMessage
	if args != "" {
		raw = json.RawMessage(args)
	}
	return app.CallTool(ctx, h.d, app.CallToolCommand{Principal: h.alice, Ref: ref, Arguments: raw})
}

func okResult(body string) upstreamStep { return upstreamStep{result: json.RawMessage(body)} }

func unauthorized() upstreamStep {
	return upstreamStep{err: kernel.NewUpstreamError(kernel.UpstreamUnauthorized)}
}

func isErrText(r domain.ToolResult) (string, bool) {
	if r.IsError == nil || !*r.IsError {
		return "", false
	}
	var blocks []struct{ Text string }
	if json.Unmarshal(r.Content, &blocks) != nil || len(blocks) != 1 {
		return "", false
	}
	return blocks[0].Text, true
}

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

func contains(s, sub string) bool { return strings.Contains(s, sub) }
