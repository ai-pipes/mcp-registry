package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// fakeUpstream is a scripted upstream: it answers tools, or fails with failure, and remembers each
// target. With hold() the next call parks until the returned release is called.
type fakeUpstream struct {
	j *journal

	mu       sync.Mutex
	tools    []kernel.UpstreamTool
	failure  error
	targets  []kernel.UpstreamTarget
	parked   chan holdAnswer
	started  chan struct{}
	duringFn func() // runs inside ListTools, before it answers
	afterFn  func() // runs inside ListTools, after the answer is known, just before it returns
}

type holdAnswer struct {
	tools []kernel.UpstreamTool
	err   error
}

func (u *fakeUpstream) ListTools(_ context.Context, t kernel.UpstreamTarget) ([]kernel.UpstreamTool, error) {
	u.mu.Lock()
	u.j.add("upstream.list")
	u.targets = append(u.targets, t)
	parked, started, during, after := u.parked, u.started, u.duringFn, u.afterFn
	u.parked = nil
	tools, failure := u.tools, u.failure
	u.mu.Unlock()
	if during != nil {
		during()
	}
	if parked != nil {
		started <- struct{}{}
		a := <-parked
		return a.tools, a.err
	}
	if after != nil {
		after()
	}
	return tools, failure
}

func (u *fakeUpstream) CallTool(context.Context, kernel.UpstreamTarget, string, json.RawMessage) (kernel.UpstreamCallResult, error) {
	return nil, errors.New("not used by sync")
}

// hold makes the next ListTools park; started fires once it did, release answers it.
func (u *fakeUpstream) hold() (started <-chan struct{}, release func(tools []kernel.UpstreamTool, err error)) {
	parked := make(chan holdAnswer, 1)
	s := make(chan struct{}, 1)
	u.mu.Lock()
	u.parked, u.started = parked, s
	u.mu.Unlock()
	return s, func(tools []kernel.UpstreamTool, err error) { parked <- holdAnswer{tools, err} }
}

func (u *fakeUpstream) targetCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.targets)
}

// fakeCredentials: personal tokens by user id; anyone else fails with failure. Both admins are active.
type fakeCredentials struct {
	j            *journal
	tokens       map[string]string
	activeAdmins map[string]bool
	failure      domain.PersonalCredentialFailure
	asked        []string
	activeErr    error
}

func (c *fakeCredentials) IsActiveAdmin(_ context.Context, userID string) (bool, error) {
	c.j.add("credentials.isActiveAdmin:" + userID)
	return c.activeAdmins[userID], c.activeErr
}

func (c *fakeCredentials) PersonalSecret(_ context.Context, _ app.ServerCallTarget, userID string) (app.PersonalCredential, error) {
	c.j.add("credentials.personalSecret:" + userID)
	c.asked = append(c.asked, userID)
	tok, ok := c.tokens[userID]
	if !ok {
		return app.PersonalCredential{Failure: c.failure}, nil
	}
	return app.PersonalCredential{Secret: kernel.NewSecret(tok)}, nil
}

type recordingLogger struct {
	mu      sync.Mutex
	records [][]any
}

func (l *recordingLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, append([]any{msg}, args...))
}

type syncHarness struct {
	*harness
	upstream *fakeUpstream
	creds    *fakeCredentials
	logger   *recordingLogger
}

func newSyncHarness() *syncHarness {
	h := newHarness()
	up := &fakeUpstream{j: h.journal}
	creds := &fakeCredentials{
		j: h.journal, tokens: map[string]string{}, activeAdmins: map[string]bool{"admin-1": true, "admin-2": true},
		failure: domain.CredentialNeedsConnection,
	}
	logger := &recordingLogger{}
	h.deps.Upstream, h.deps.Credentials, h.deps.Logger = up, creds, logger
	return &syncHarness{harness: h, upstream: up, creds: creds, logger: logger}
}

func utool(name string, mut ...func(*kernel.UpstreamTool)) kernel.UpstreamTool {
	desc := "does " + name
	t := kernel.UpstreamTool{Name: name, Description: &desc, InputSchema: []byte(`{"type":"object"}`)}
	for _, m := range mut {
		m(&t)
	}
	return t
}

func (h *syncHarness) seed(t *testing.T, auth domain.AuthMode, slug string, secret *string) {
	t.Helper()
	h.mustCreate(t, slug, secret, func(f *app.ServerFields) {
		f.Auth, f.URL, f.TimeoutMs = auth, "https://mcp.example.com/"+slug, ptr(5000)
	})
	h.clean()
}

func (h *syncHarness) sync(p *app.Principal, slug string) (app.SyncCounts, error) {
	return app.SyncServer(ctx, h.deps, app.SyncCommand{Principal: p, Slug: slug})
}

func (h *syncHarness) server(t *testing.T, slug string) domain.ServerState {
	t.Helper()
	return h.servers.row(t, slug)
}

func (h *syncHarness) eventsOf() [][2]any {
	out := [][2]any{}
	for _, e := range *h.outbox {
		out = append(out, [2]any{e.Type, e.Payload})
	}
	return out
}

func TestSyncServerSucceeds(t *testing.T) {
	t.Run("adds new tools: counts, event, target, sync state, audit", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("b", func(u *kernel.UpstreamTool) { u.Annotations = []byte(`{"readOnlyHint":true}`) })}
		counts, err := h.sync(&admin, "gitea")
		if err != nil || counts != (app.SyncCounts{Added: 2}) {
			t.Fatalf("counts = %+v, err = %v", counts, err)
		}
		want := [][2]any{{"catalog.tools_changed", map[string]any{"server": "gitea", "refs": []string{"gitea.a", "gitea.b"}}}}
		if got := h.eventsOf(); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v", got)
		}
		b, _ := h.tools.row("gitea", "b")
		if b.Annotations.ReadOnlyHint == nil || !*b.Annotations.ReadOnlyHint {
			t.Fatalf("annotations = %+v", b.Annotations)
		}
		if len(h.upstream.targets) != 1 {
			t.Fatalf("targets = %v", h.upstream.targets)
		}
		tg := h.upstream.targets[0]
		if tg.URL != "https://mcp.example.com/gitea" || len(tg.Headers) != 0 || tg.Headers == nil || tg.Timeout != 5*time.Second || tg.MaxResponseBytes != 1234 {
			t.Fatalf("target = %+v", tg)
		}
		st := h.server(t, "gitea")
		if st.LastSyncAt == nil || !st.LastSyncAt.Equal(t0) || st.LastSyncError != nil {
			t.Fatalf("state = %+v", st)
		}
		wantAudit := []kernel.AuditEntry{{ActorID: ptr("admin-1"), Action: "server.sync", Target: ptr("gitea"), Meta: map[string]any{"added": 2, "changed": 0, "removed": 0}}}
		if !reflect.DeepEqual(h.audit.entries, wantAudit) {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
	})

	t.Run("a changed description is a change of that tool only", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("b")}
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		h.clean()
		newText := "new text"
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("b", func(u *kernel.UpstreamTool) { u.Description = &newText })}
		counts, err := h.sync(nil, "gitea")
		if err != nil || counts != (app.SyncCounts{Changed: 1}) {
			t.Fatalf("counts = %+v, err = %v", counts, err)
		}
		want := [][2]any{{"catalog.tools_changed", map[string]any{"server": "gitea", "refs": []string{"gitea.b"}}}}
		if got := h.eventsOf(); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v", got)
		}
		if b, _ := h.tools.row("gitea", "b"); b.Description != "new text" {
			t.Fatalf("description = %q", b.Description)
		}
		if len(h.audit.entries) != 0 {
			t.Fatal("the scheduler writes no audit record")
		}
	})

	t.Run("a vanished tool is marked removed", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("b")}
		_, _ = h.sync(&admin, "gitea")
		h.clean()
		h.clock.set(t1)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		counts, err := h.sync(&admin, "gitea")
		if err != nil || counts != (app.SyncCounts{Removed: 1}) {
			t.Fatalf("counts = %+v, err = %v", counts, err)
		}
		if b, _ := h.tools.row("gitea", "b"); b.RemovedAt == nil || !b.RemovedAt.Equal(t1) {
			t.Fatalf("b = %+v", b)
		}
		want := [][2]any{{"catalog.tools_removed", map[string]any{"server": "gitea", "refs": []string{"gitea.b"}}}}
		if got := h.eventsOf(); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("a returning tool keeps hidden", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("drop_repo")}
		_, _ = h.sync(&admin, "gitea")
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.drop_repo", Hidden: true}), "ok")
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		_, _ = h.sync(&admin, "gitea")
		h.upstream.tools = []kernel.UpstreamTool{utool("a"), utool("drop_repo")}
		counts, err := h.sync(&admin, "gitea")
		if err != nil || counts != (app.SyncCounts{Added: 1}) {
			t.Fatalf("counts = %+v, err = %v", counts, err)
		}
		if d, _ := h.tools.row("gitea", "drop_repo"); !d.Hidden || d.RemovedAt != nil {
			t.Fatalf("tool = %+v", d)
		}
	})

	t.Run("a later success clears last_sync_error", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = kernel.NewUpstreamError(kernel.UpstreamTransport)
		_, _ = h.sync(nil, "gitea")
		if h.server(t, "gitea").LastSyncError == nil {
			t.Fatal("error not stored")
		}
		h.upstream.failure = nil
		if _, err := h.sync(nil, "gitea"); err != nil {
			t.Fatal(err)
		}
		if h.server(t, "gitea").LastSyncError != nil {
			t.Fatal("error not cleared")
		}
	})

	t.Run("the attempt's start is recorded, not the time the answer came", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		h.upstream.duringFn = func() { h.clock.set(t2) }
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		if st := h.server(t, "gitea"); !st.LastSyncAt.Equal(t0) {
			t.Fatalf("last_sync_at = %v, want the start %v", st.LastSyncAt, t0)
		}
		if a, _ := h.tools.row("gitea", "a"); !a.SyncedAt.Equal(t2) {
			t.Fatalf("synced_at = %v, want %v", a.SyncedAt, t2)
		}
	})
}

// The failure matrix: every kind of upstream error has its own stored text and audit code, and
// the reply is sync_failed with that text.
func TestSyncServerUpstreamFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string // audit meta.error
		text string
	}{
		{"unauthorized", kernel.NewUpstreamError(kernel.UpstreamUnauthorized), "unauthorized", "Upstream rejected the credentials (HTTP 401)"},
		{"timeout", kernel.NewUpstreamError(kernel.UpstreamTimeout), "timeout", "Upstream did not respond in time"},
		{"too large", kernel.NewUpstreamError(kernel.UpstreamTooLarge), "too_large", "The upstream response exceeds the allowed size"},
		{"transport", kernel.NewUpstreamError(kernel.UpstreamTransport), "transport", "Could not reach the upstream"},
		{"rejected", kernel.RejectedUpstreamError(-32601, "Method not found: tools/list secret-xyz"), "rejected", "Upstream answered tools/list with a JSON-RPC error"},
		{"an unknown kind reads as transport", &kernel.UpstreamError{Kind: "weird"}, "transport", "Could not reach the upstream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSyncHarness()
			h.seed(t, authNone, "gitea", nil)
			h.upstream.tools = []kernel.UpstreamTool{utool("a")}
			if _, err := h.sync(&admin, "gitea"); err != nil {
				t.Fatal(err)
			}
			h.clean()
			h.clock.set(t1)
			before, _ := h.tools.row("gitea", "a")
			h.upstream.failure = c.err
			_, err := h.sync(&admin, "gitea")
			wantError(t, err, "sync_failed", c.text)
			st := h.server(t, "gitea")
			if st.LastSyncError == nil || *st.LastSyncError != c.text || !st.LastSyncAt.Equal(t1) {
				t.Fatalf("state = %+v", st)
			}
			if after, _ := h.tools.row("gitea", "a"); !reflect.DeepEqual(before, after) {
				t.Fatal("tools changed on failure")
			}
			if len(*h.outbox) != 0 || len(h.tools.writes) != 0 {
				t.Fatal("events or tool writes on failure")
			}
			want := []kernel.AuditEntry{{ActorID: ptr("admin-1"), Action: "server.sync", Target: ptr("gitea"), Meta: map[string]any{"error": c.code}}}
			if !reflect.DeepEqual(h.audit.entries, want) {
				t.Fatalf("audit = %+v", h.audit.entries)
			}
			if strings.Contains(err.Error(), "secret-xyz") || strings.Contains(*st.LastSyncError, "secret-xyz") {
				t.Fatal("the upstream's message leaked")
			}
		})
	}
}

func TestSyncServerOrderOfSteps(t *testing.T) {
	t.Run("success: credentials, tools/list, tools, sync state, audit", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authPAT, "gitea", nil)
		h.creds.tokens["admin-1"] = "pat-1"
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		equalStrings(t, h.journal.all(), []string{
			"credentials.isActiveAdmin:admin-1", "credentials.personalSecret:admin-1", "upstream.list",
			"tools.apply:gitea", "servers.syncstate:gitea", "audit:server.sync",
		})
	})

	t.Run("failure: no tools are written; sync state, then audit", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = kernel.NewUpstreamError(kernel.UpstreamTimeout)
		_, _ = h.sync(&admin, "gitea")
		equalStrings(t, h.journal.all(), []string{"upstream.list", "servers.syncstate:gitea", "audit:server.sync"})
	})

	t.Run("a refusal before the upstream: sync state and audit, upstream not contacted", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authShared, "gitea", nil)
		_, _ = h.sync(&admin, "gitea")
		equalStrings(t, h.journal.all(), []string{"servers.syncstate:gitea", "audit:server.sync"})
	})

	t.Run("the start time is read before the admin check", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		cc := &countingClock{t: t0}
		h.deps.Clock = cc
		_, err := h.sync(ptr(member()), "gitea")
		wantCode(t, err, "forbidden")
		if cc.n != 1 {
			t.Fatalf("clock reads = %d, want 1 (the start, before the check)", cc.n)
		}
	})
}

type countingClock struct {
	t time.Time
	n int
}

func (c *countingClock) Now() time.Time { c.n++; return c.t }

func TestSyncServerCredentials(t *testing.T) {
	t.Run("shared_secret sends the server secret in the configured header", func(t *testing.T) {
		h := newSyncHarness()
		auth := authShared
		auth.Template = "k {secret}"
		h.seed(t, auth, "gitea", ptr("s3cret"))
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		if got := h.upstream.targets[0].Headers; len(got) != 1 || got["X-API-Key"].Reveal() != "k s3cret" {
			t.Fatalf("headers = %v", got)
		}
	})

	t.Run("shared_secret without a secret fails with no_secret, upstream not contacted", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authShared, "gitea", nil)
		_, err := h.sync(&admin, "gitea")
		wantError(t, err, "sync_failed", "The server secret is not set")
		if h.upstream.targetCount() != 0 {
			t.Fatal("upstream contacted")
		}
		if got := *h.server(t, "gitea").LastSyncError; got != "The server secret is not set" {
			t.Fatalf("stored = %q", got)
		}
		if h.audit.entries[0].Meta["error"] != "no_secret" {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
	})

	t.Run("a secret no key opens is not a no_secret failure: the error comes out unrecorded", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authShared, "gitea", ptr("s3cret"))
		h.deps.Box = fakeBox{id: "other"}
		_, err := h.sync(&admin, "gitea")
		if err == nil || code(err) != "<not a DomainError: wrong key>" {
			t.Fatalf("err = %v", err)
		}
		if h.server(t, "gitea").LastSyncError != nil || len(h.audit.entries) != 0 {
			t.Fatal("recorded by SyncServer itself")
		}
	})

	t.Run("oauth_user without the admin's connection: needs_connection, catalog untouched", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		_, err := h.sync(&admin, "gitea")
		wantError(t, err, "needs_connection", "An administrator needs to connect this server")
		equalStrings(t, h.creds.asked, []string{"admin-1"})
		if h.upstream.targetCount() != 0 {
			t.Fatal("upstream contacted")
		}
		if _, ok := h.tools.row("gitea", "a"); ok || len(*h.outbox) != 0 {
			t.Fatal("catalog touched")
		}
		if got := *h.server(t, "gitea").LastSyncError; got != "An administrator needs to connect this server" {
			t.Fatalf("stored = %q", got)
		}
	})

	t.Run("pat_user: the catalog builds the header from the personal secret", func(t *testing.T) {
		h := newSyncHarness()
		auth := authPAT
		auth.Template = "token {secret}"
		h.seed(t, auth, "gitea", nil)
		h.creds.tokens["admin-1"] = "pat-1"
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		if got := h.upstream.targets[0].Headers; got["Authorization"].Reveal() != "token pat-1" {
			t.Fatalf("headers = %v", got)
		}
	})

	t.Run("oauth_user: the starting admin's token, and the scheduler remembers them", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.tokens["admin-1"], h.creds.tokens["admin-2"] = "tok-1", "tok-2"
		for _, p := range []*app.Principal{&admin, &admin2, nil} {
			if _, err := h.sync(p, "gitea"); err != nil {
				t.Fatal(err)
			}
		}
		equalStrings(t, h.creds.asked, []string{"admin-1", "admin-2", "admin-2"})
		var got []string
		for _, tg := range h.upstream.targets {
			got = append(got, tg.Headers["Authorization"].Reveal())
		}
		equalStrings(t, got, []string{"Bearer tok-1", "Bearer tok-2", "Bearer tok-2"})
		if u := h.server(t, "gitea").SyncUserID; u == nil || *u != "admin-2" {
			t.Fatalf("sync user = %v", u)
		}
	})

	t.Run("the scheduler on an oauth_user server nobody synchronised: needs_connection, no audit", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		_, err := h.sync(nil, "gitea")
		wantCode(t, err, "needs_connection")
		if len(h.creds.asked) != 0 || len(h.audit.entries) != 0 {
			t.Fatalf("asked = %v, audit = %v", h.creds.asked, h.audit.entries)
		}
		if got := *h.server(t, "gitea").LastSyncError; got != "An administrator needs to connect this server" {
			t.Fatalf("stored = %q", got)
		}
		equalStrings(t, h.journal.all(), []string{"servers.syncstate:gitea"})
	})

	t.Run("the remembered admin, demoted or disabled since: their connection is not used", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.tokens["admin-1"] = "tok-1"
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		delete(h.creds.activeAdmins, "admin-1")
		_, err := h.sync(nil, "gitea")
		wantCode(t, err, "needs_connection")
		equalStrings(t, h.creds.asked, []string{"admin-1"})
		if h.upstream.targetCount() != 1 {
			t.Fatal("the demoted admin's token was used")
		}
	})

	t.Run("an expired connection needs a new one; an unreachable provider is its own failure", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.failure = domain.CredentialExpired
		_, err := h.sync(&admin, "gitea")
		wantCode(t, err, "needs_connection")
		h.creds.failure = domain.CredentialUnavailable
		_, err = h.sync(&admin, "gitea")
		wantError(t, err, "sync_failed", "The OAuth provider is unavailable")
		if got := *h.server(t, "gitea").LastSyncError; got != "The OAuth provider is unavailable" {
			t.Fatalf("stored = %q", got)
		}
		if h.upstream.targetCount() != 0 {
			t.Fatal("upstream contacted")
		}
		if h.audit.entries[1].Meta["error"] != "credentials_unavailable" {
			t.Fatalf("audit = %+v", h.audit.entries[1])
		}
	})

	t.Run("a failed sync does not replace the remembered admin", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.tokens["admin-1"] = "tok-1"
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.sync(&admin2, "gitea"); err == nil {
			t.Fatal("admin-2 has no connection")
		}
		if u := h.server(t, "gitea").SyncUserID; u == nil || *u != "admin-1" {
			t.Fatalf("sync user = %v", u)
		}
	})

	t.Run("errors of the credentials port are unexpected: returned, not recorded", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.activeErr = errBoom{}
		if _, err := h.sync(&admin, "gitea"); err != (errBoom{}) {
			t.Fatalf("err = %v", err)
		}
		if h.server(t, "gitea").LastSyncError != nil {
			t.Fatal("recorded")
		}
	})
}

func TestSyncServerRefusals(t *testing.T) {
	h := newSyncHarness()
	h.seed(t, authNone, "gitea", nil)
	_, err := h.sync(ptr(member()), "gitea")
	wantError(t, err, "forbidden", "administrators only")
	_, err = h.sync(&admin, "nope")
	wantError(t, err, "not_found", `active server "nope" not found`)
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}), "ok")
	h.clean()
	_, err = h.sync(nil, "gitea")
	wantError(t, err, "not_found", `active server "gitea" not found`)
	if h.upstream.targetCount() != 0 || len(h.audit.entries) != 0 || len(h.journal.all()) != 0 {
		t.Fatalf("a refused sync did something: %v", h.journal.all())
	}
	// A non-upstream error from the upstream client is not swallowed by SyncServer.
	h2 := newSyncHarness()
	h2.seed(t, authNone, "gitea", nil)
	h2.upstream.failure = errBoom{}
	if _, err := h2.sync(&admin, "gitea"); err != (errBoom{}) {
		t.Fatalf("err = %v", err)
	}
	if h2.server(t, "gitea").LastSyncError != nil {
		t.Fatal("SyncServer recorded an unexpected error")
	}
}

func TestSyncServerOverlappingRuns(t *testing.T) {
	t.Run("a scheduler run that finishes after an admin's newer sync does not undo it", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authOAuth, "gitea", nil)
		h.creds.tokens["admin-1"], h.creds.tokens["admin-2"] = "tok-1", "tok-2"
		if _, err := h.sync(&admin, "gitea"); err != nil {
			t.Fatal(err)
		}
		h.clock.set(t1)
		started, release := h.upstream.hold()
		done := make(chan error, 1)
		go func() { _, err := h.sync(nil, "gitea"); done <- err }()
		<-started
		h.clock.set(t2)
		if _, err := h.sync(&admin2, "gitea"); err != nil {
			t.Fatal(err)
		}
		release([]kernel.UpstreamTool{utool("a")}, nil)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		st := h.server(t, "gitea")
		if st.SyncUserID == nil || *st.SyncUserID != "admin-2" || !st.LastSyncAt.Equal(t2) || st.LastSyncError != nil {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("a late failure of an older run does not overwrite a newer success", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.clock.set(t1)
		started, release := h.upstream.hold()
		done := make(chan error, 1)
		go func() { _, err := h.sync(&admin, "gitea"); done <- err }()
		<-started
		h.clock.set(t2)
		if _, err := h.sync(&admin2, "gitea"); err != nil {
			t.Fatal(err)
		}
		release(nil, kernel.NewUpstreamError(kernel.UpstreamTimeout))
		wantCode(t, <-done, "sync_failed")
		st := h.server(t, "gitea")
		if st.SyncUserID == nil || *st.SyncUserID != "admin-2" || !st.LastSyncAt.Equal(t2) || st.LastSyncError != nil {
			t.Fatalf("state = %+v", st)
		}
	})
}

var t1, t2 = t0.Add(time.Hour), t0.Add(2 * time.Hour)

func TestSyncServerRecordingFailure(t *testing.T) {
	t.Run("an unexpected error is logged without its message and recorded as internal", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = errors.New("boom token=sekret")
		_, err := app.SyncServerRecordingFailure(ctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		wantError(t, err, "internal", "Internal sync error")
		if got := *h.server(t, "gitea").LastSyncError; got != "Internal sync error" {
			t.Fatalf("stored = %q", got)
		}
		if len(h.logger.records) != 1 || h.logger.records[0][0] != "catalog sync failed" {
			t.Fatalf("log = %v", h.logger.records)
		}
		if strings.Contains(strings.Join(flat(h.logger.records), " "), "sekret") {
			t.Fatalf("the message leaked into the log: %v", h.logger.records)
		}
	})

	t.Run("an expected refusal passes through and is not logged", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = kernel.NewUpstreamError(kernel.UpstreamTransport)
		_, err := app.SyncServerRecordingFailure(ctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		wantCode(t, err, "sync_failed")
		_, err = app.SyncServerRecordingFailure(ctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "nope"})
		wantCode(t, err, "not_found")
		if len(h.logger.records) != 0 {
			t.Fatalf("log = %v", h.logger.records)
		}
	})

	t.Run("a failure to save the sync state is logged too, and the answer is still internal", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = errBoom{}
		h.servers.failSyncState = errBoom{}
		_, err := app.SyncServerRecordingFailure(ctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		wantCode(t, err, "internal")
		if len(h.logger.records) != 2 || h.logger.records[1][0] != "sync state not saved" {
			t.Fatalf("log = %v", h.logger.records)
		}
	})

	t.Run("a cancelled context is returned as it is, not recorded as a failure of the server", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		cctx, cancel := context.WithCancel(ctx)
		h.upstream.failure = context.Canceled
		h.upstream.duringFn = cancel
		_, err := app.SyncServerRecordingFailure(cctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if h.server(t, "gitea").LastSyncError != nil || len(h.logger.records) != 0 {
			t.Fatal("a shutdown was recorded as an internal failure")
		}
	})

	t.Run("works without a logger", func(t *testing.T) {
		h := newSyncHarness()
		h.deps.Logger = nil
		h.seed(t, authNone, "gitea", nil)
		h.upstream.failure = errBoom{}
		_, err := app.SyncServerRecordingFailure(ctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		wantCode(t, err, "internal")
	})
}

func flat(records [][]any) []string {
	var out []string
	for _, r := range records {
		for _, a := range r {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestSyncAllServers(t *testing.T) {
	t.Run("synchronises every active server and reports each outcome", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "alpha", nil)
		h.seed(t, authOAuth, "beta", nil)
		h.seed(t, authNone, "gamma", nil)
		wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gamma", Status: domain.StatusDisabled}), "ok")
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		out, err := app.SyncAllServers(ctx, h.deps)
		if err != nil {
			t.Fatal(err)
		}
		want := []app.SyncOutcome{{Slug: "alpha", OK: true}, {Slug: "beta", OK: false, Code: "needs_connection"}}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("outcomes = %+v", out)
		}
		if h.upstream.targetCount() != 1 || h.upstream.targets[0].URL != "https://mcp.example.com/alpha" {
			t.Fatalf("targets = %+v", h.upstream.targets)
		}
	})

	t.Run("an unexpected error on one server is recorded as internal; the rest still run", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "alpha", nil)
		h.seed(t, authShared, "beta", ptr("s3cret"))
		h.seed(t, authNone, "gamma", nil)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		// beta's secret cannot be opened (sealed under a lost key).
		h.deps.Box = openFailsFor{fakeBox: fakeBox{id: "k1"}, plain: "s3cret"}
		out, err := app.SyncAllServers(ctx, h.deps)
		if err != nil {
			t.Fatal(err)
		}
		want := []app.SyncOutcome{{Slug: "alpha", OK: true}, {Slug: "beta", OK: false, Code: "internal"}, {Slug: "gamma", OK: true}}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("outcomes = %+v", out)
		}
		if got := *h.server(t, "beta").LastSyncError; got != "Internal sync error" {
			t.Fatalf("stored = %q", got)
		}
		if len(h.logger.records) != 1 || !contains(h.logger.records[0], "beta") {
			t.Fatalf("log = %v", h.logger.records)
		}
		if strings.Contains(strings.Join(flat(h.logger.records), " "), "s3cret") {
			t.Fatal("secret in the log")
		}
		// The scheduler is not an admin: no audit records.
		if len(h.audit.entries) != 0 {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
	})

	t.Run("a cancelled context stops the run and is returned", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "alpha", nil)
		h.seed(t, authNone, "beta", nil)
		cctx, cancel := context.WithCancel(ctx)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		h.upstream.duringFn = cancel
		out, err := app.SyncAllServers(cctx, h.deps)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(out) > 1 {
			t.Fatalf("outcomes = %+v", out)
		}
	})

	t.Run("a failure to list the servers is returned", func(t *testing.T) {
		h := newSyncHarness()
		h.deps.Servers = failingList{h.servers}
		if _, err := app.SyncAllServers(ctx, h.deps); err != (errBoom{}) {
			t.Fatalf("err = %v", err)
		}
	})
}

type failingList struct{ *fakeServers }

func (failingList) List(context.Context) ([]app.ServerListing, error) { return nil, errBoom{} }

type openFailsFor struct {
	fakeBox
	plain string
}

func (b openFailsFor) Open(sealed []byte) (string, error) {
	p, err := b.fakeBox.Open(sealed)
	if err == nil && p == b.plain {
		return "", errors.New("unable to authenticate data: " + p)
	}
	return p, err
}

func contains(rec []any, s string) bool {
	for _, a := range rec {
		if a == s {
			return true
		}
	}
	return false
}

func TestRegisterServer(t *testing.T) {
	t.Run("creates the server and synchronises it as the creating admin", func(t *testing.T) {
		h := newSyncHarness()
		h.upstream.tools = []kernel.UpstreamTool{utool("ping")}
		v, err := app.RegisterServer(ctx, h.deps, createCmd("gitea", nil))
		if err != nil || v.Slug != "gitea" || v.ToolCount != 1 || v.LastSyncError != nil || v.LastSyncAt == nil {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
		equalStrings(t, h.audit.actions(), []string{"server.create", "server.sync"})
		if h.audit.entries[1].ActorID == nil || *h.audit.entries[1].ActorID != "admin-1" {
			t.Fatalf("audit = %+v", h.audit.entries[1])
		}
	})

	t.Run("audit order with a secret: create, secret_set, sync", func(t *testing.T) {
		h := newSyncHarness()
		h.upstream.tools = []kernel.UpstreamTool{utool("ping")}
		_, err := app.RegisterServer(ctx, h.deps, createCmd("gitea", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared }))
		if err != nil {
			t.Fatal(err)
		}
		equalStrings(t, h.audit.actions(), []string{"server.create", "server.secret_set", "server.sync"})
	})

	t.Run("a failed sync keeps the server and reports the error text in the view", func(t *testing.T) {
		h := newSyncHarness()
		h.upstream.failure = kernel.NewUpstreamError(kernel.UpstreamTransport)
		v, err := app.RegisterServer(ctx, h.deps, createCmd("gitea", nil))
		if err != nil {
			t.Fatalf("a failed sync must not fail the registration: %v", err)
		}
		if v.ToolCount != 0 || v.LastSyncError == nil || *v.LastSyncError != "Could not reach the upstream" || v.LastSyncAt == nil {
			t.Fatalf("view = %+v", v)
		}
		if _, ok := h.servers.rows["gitea"]; !ok {
			t.Fatal("not created")
		}
	})

	t.Run("an unexpected sync error is absorbed: internal text in the view, logged once", func(t *testing.T) {
		h := newSyncHarness()
		h.upstream.failure = errors.New("boom token=sekret")
		v, err := app.RegisterServer(ctx, h.deps, createCmd("gitea", nil))
		if err != nil || v.LastSyncError == nil || *v.LastSyncError != "Internal sync error" {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
		if len(h.logger.records) != 1 || strings.Contains(strings.Join(flat(h.logger.records), " "), "sekret") {
			t.Fatalf("log = %v", h.logger.records)
		}
	})

	t.Run("a context that ends during the first sync keeps the created server and records nothing about the sync", func(t *testing.T) {
		h := newSyncHarness()
		h.upstream.failure = context.Canceled
		cctx, cancel := context.WithCancel(ctx)
		cancel() // the creation is written whole all the same
		v, err := app.RegisterServer(cctx, h.deps, createCmd("gitea", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared }))
		if !errors.Is(err, app.ErrFirstSyncInterrupted) || v.Slug != "gitea" || v.LastSyncAt != nil || v.LastSyncError != nil {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
		equalStrings(t, h.audit.actions(), []string{"server.create", "server.secret_set"})
	})

	t.Run("a personal-auth server fails its first sync with needs_connection, the server stays", func(t *testing.T) {
		h := newSyncHarness()
		v, err := app.RegisterServer(ctx, h.deps, createCmd("gitea", nil, func(f *app.ServerFields) { f.Auth = authOAuth }))
		if err != nil || v.LastSyncError == nil || *v.LastSyncError != "An administrator needs to connect this server" {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
	})

	t.Run("creates nothing for a member, and a refused create does not sync", func(t *testing.T) {
		h := newSyncHarness()
		_, err := app.RegisterServer(ctx, h.deps, app.CreateServerCommand{Principal: member(), Input: input("gitea")})
		wantCode(t, err, "forbidden")
		_, err = app.RegisterServer(ctx, h.deps, createCmd("Bad", nil))
		wantCode(t, err, "invalid_slug")
		if len(h.servers.rows) != 0 || h.upstream.targetCount() != 0 {
			t.Fatal("something happened")
		}
	})
}

func TestSyncCancelledAfterTheAnswerStillWritesEverything(t *testing.T) {
	t.Run("success: tools, sync state and audit all land", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		cctx, cancel := context.WithCancel(ctx)
		h.upstream.tools = []kernel.UpstreamTool{utool("a")}
		h.upstream.afterFn = cancel
		counts, err := app.SyncServer(cctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		if err != nil || counts.Added != 1 {
			t.Fatalf("counts = %+v, err = %v", counts, err)
		}
		equalStrings(t, h.journal.all(), []string{"upstream.list", "tools.apply:gitea", "servers.syncstate:gitea", "audit:server.sync"})
	})

	t.Run("failure: the failure record and the audit land", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		cctx, cancel := context.WithCancel(ctx)
		h.upstream.failure = kernel.NewUpstreamError(kernel.UpstreamTransport)
		h.upstream.afterFn = cancel
		_, err := app.SyncServer(cctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		wantCode(t, err, "sync_failed")
		equalStrings(t, h.journal.all(), []string{"upstream.list", "servers.syncstate:gitea", "audit:server.sync"})
	})

	t.Run("cancelled before tools/list: nothing is recorded", func(t *testing.T) {
		h := newSyncHarness()
		h.seed(t, authNone, "gitea", nil)
		cctx, cancel := context.WithCancel(ctx)
		h.upstream.failure = context.Canceled
		h.upstream.duringFn = cancel
		_, err := app.SyncServer(cctx, h.deps, app.SyncCommand{Principal: &admin, Slug: "gitea"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		equalStrings(t, h.journal.all(), []string{"upstream.list"})
	})
}

type panickingServers struct {
	*fakeServers
	slug string
}

func (p panickingServers) FindBySlug(c context.Context, slug string) (*domain.Server, error) {
	if slug == p.slug {
		panic("boom token=sekret")
	}
	return p.fakeServers.FindBySlug(c, slug)
}

func TestAPanickingSyncIsRecordedAsInternal(t *testing.T) {
	h := newSyncHarness()
	h.seed(t, authNone, "alpha", nil)
	h.seed(t, authNone, "beta", nil)
	h.seed(t, authNone, "gamma", nil)
	h.upstream.tools = []kernel.UpstreamTool{utool("a")}
	h.deps.Servers = panickingServers{h.servers, "beta"}
	out, err := app.SyncAllServers(ctx, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []app.SyncOutcome{{Slug: "alpha", OK: true}, {Slug: "beta", Code: "internal"}, {Slug: "gamma", OK: true}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("outcomes = %+v", out)
	}
	if got := h.servers.row(t, "beta").LastSyncError; got == nil || *got != "Internal sync error" {
		t.Fatalf("stored = %v", got)
	}
	if len(h.logger.records) != 1 || h.logger.records[0][0] != "catalog sync failed" || contains(h.logger.records[0], "beta") == false {
		t.Fatalf("log = %v", h.logger.records)
	}
	if strings.Contains(strings.Join(flat(h.logger.records), " "), "sekret") {
		t.Fatal("the panic value was logged")
	}
}
