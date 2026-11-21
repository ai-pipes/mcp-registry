package app_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

func TestAdminOnlyCommandsRefuseAMemberAndChangeNothing(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
	h.mustApply(t, "gitea", snap("list"))
	h.clean()
	p := member()

	_, err := app.CreateServer(ctx, h.deps, app.CreateServerCommand{Principal: p, Input: input("other")})
	wantError(t, err, "forbidden", "administrators only")
	_, err = app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: p, Slug: "gitea", Fields: fields()})
	wantCode(t, err, "forbidden")
	wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: p, Slug: "gitea", Secret: ptr("s")}), "forbidden")
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: p, Slug: "gitea", Status: domain.StatusDisabled}), "forbidden")
	wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: p, Ref: "gitea.list", Hidden: true}), "forbidden")
	_, err = app.ListAllServers(ctx, h.deps, p)
	wantCode(t, err, "forbidden")
	_, err = app.GetServer(ctx, h.deps, p, "gitea")
	wantCode(t, err, "forbidden")
	_, err = app.ListServerTools(ctx, h.deps, p, "gitea")
	wantCode(t, err, "forbidden")

	if len(h.servers.rows) != 1 || len(*h.outbox) != 0 || len(h.audit.entries) != 0 || len(h.servers.writes) != 0 {
		t.Fatalf("a refused command changed something: rows=%d events=%d audit=%d", len(h.servers.rows), len(*h.outbox), len(h.audit.entries))
	}
}

func TestCreateServer(t *testing.T) {
	t.Run("creates, audits, and writes server_changed with the aggregate in one call", func(t *testing.T) {
		h := newHarness()
		v, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil, func(f *app.ServerFields) {
			f.TimeoutMs, f.DailyLimit = ptr(5000), ptr(3)
		}))
		if err != nil {
			t.Fatal(err)
		}
		if v.Slug != "gitea" || v.Status != domain.StatusActive || v.SecretSet || v.TimeoutMs != 5000 || v.ToolCount != 0 ||
			v.DailyLimit == nil || *v.DailyLimit != 3 {
			t.Fatalf("view = %+v", v)
		}
		want := []kernel.AuditEntry{{ActorID: ptr("admin-1"), Action: "server.create", Target: ptr("gitea"), Meta: map[string]any{"auth": "none"}}}
		if !reflect.DeepEqual(h.audit.entries, want) {
			t.Fatalf("audit = %+v, want %+v", h.audit.entries, want)
		}
		if len(h.servers.writes) != 1 {
			t.Fatalf("writes = %+v", h.servers.writes)
		}
		w := h.servers.writes[0]
		if w.Op != "create" || len(w.Events) != 1 || w.Events[0].Type != "catalog.server_changed" ||
			!reflect.DeepEqual(w.Events[0].Payload, map[string]any{"server": "gitea"}) {
			t.Fatalf("the aggregate and its event must be one write: %+v", w)
		}
	})

	t.Run("defaults: timeout 120000, no daily limit", func(t *testing.T) {
		h := newHarness()
		v, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil))
		if err != nil {
			t.Fatal(err)
		}
		if v.TimeoutMs != 120000 || v.DailyLimit != nil {
			t.Fatalf("view = %+v", v)
		}
	})

	t.Run("seals an initial secret; audit has server.secret_set without the value", func(t *testing.T) {
		h := newHarness()
		v, err := app.CreateServer(ctx, h.deps, createCmd("gitea", ptr("hunter2"), func(f *app.ServerFields) { f.Auth = authShared }))
		if err != nil || !v.SecretSet {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
		equalStrings(t, h.audit.actions(), []string{"server.create", "server.secret_set"})
		if h.audit.entries[1].Meta != nil {
			t.Fatalf("secret_set carries meta: %+v", h.audit.entries[1])
		}
		if !h.servers.writes[0].SecretWritten {
			t.Fatal("a new secret must be written")
		}
		blob, _ := json.Marshal([]any{h.audit.entries, v})
		if strings.Contains(string(blob), "hunter2") || strings.Contains(string(blob), "aHVudGVy") {
			t.Fatalf("secret leaked: %s", blob)
		}
		got, ok, err := app.GetServerSecret(ctx, h.deps, "gitea")
		if err != nil || !ok || got != "hunter2" {
			t.Fatalf("GetServerSecret = %q %v %v", got, ok, err)
		}
	})

	t.Run("domain errors, nothing is written", func(t *testing.T) {
		h := newHarness()
		cases := []struct {
			name string
			cmd  app.CreateServerCommand
			code string
		}{
			{"slug", createCmd("Bad", nil), "invalid_slug"},
			{"url", createCmd("gitea", nil, func(f *app.ServerFields) { f.URL = "nope" }), "invalid_request"},
			{"auth", createCmd("gitea", nil, func(f *app.ServerFields) {
				f.Auth = domain.AuthMode{Kind: domain.AuthPATUser, Header: "A", Template: "no"}
			}), "invalid_auth"},
			{"blank secret", createCmd("gitea", ptr("  "), func(f *app.ServerFields) { f.Auth = authShared }), "invalid_server"},
			{"secret on a mode without one", createCmd("gitea", ptr("k")), "invalid_server"},
			{"blank name", createCmd("gitea", nil, func(f *app.ServerFields) { f.Name = "   " }), "invalid_server"},
			{"blank group", createCmd("gitea", nil, func(f *app.ServerFields) { f.Access = groups("  ") }), "invalid_access"},
		}
		for _, c := range cases {
			_, err := app.CreateServer(ctx, h.deps, c.cmd)
			if code(err) != c.code {
				t.Errorf("%s: error = %v, want code %s", c.name, err, c.code)
			}
		}
		if len(h.servers.rows) != 0 || len(*h.outbox) != 0 || len(h.audit.entries) != 0 {
			t.Fatal("a refused create changed something")
		}
	})

	t.Run("a taken slug is server_exists", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		h.audit.reset()
		_, err := app.CreateServer(ctx, h.deps, createCmd("gitea", nil))
		wantCode(t, err, "server_exists")
		if len(h.audit.entries) != 0 {
			t.Fatal("audit written for a refused create")
		}
	})
}

func TestUpdateServer(t *testing.T) {
	t.Run("applies the edit; audit lists all seven fields in the fixed order", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
		h.mustApply(t, "gitea", snap("a"), snap("b"))
		h.clean()
		v, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) {
			f.Name, f.Auth, f.DailyLimit = "Gitea!", authShared, ptr(7)
		})})
		if err != nil {
			t.Fatal(err)
		}
		if v.Name != "Gitea!" || v.DailyLimit == nil || *v.DailyLimit != 7 || v.ToolCount != 2 {
			t.Fatalf("view = %+v", v)
		}
		want := []kernel.AuditEntry{{
			ActorID: ptr("admin-1"), Action: "server.update", Target: ptr("gitea"),
			Meta: map[string]any{"fields": []string{"name", "description", "url", "access", "timeoutMs", "dailyLimit", "auth"}},
		}}
		if !reflect.DeepEqual(h.audit.entries, want) {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
		if got := h.eventTypes(); !reflect.DeepEqual(got, []string{"catalog.server_changed"}) {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("a PUT that omits timeout and daily limit resets them to the defaults", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.TimeoutMs, f.DailyLimit = ptr(5000), ptr(9) })
		v, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields()})
		if err != nil || v.TimeoutMs != 120000 || v.DailyLimit != nil {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
	})

	t.Run("unknown server: not_found with the quoted slug; a bad body is judged first", func(t *testing.T) {
		h := newHarness()
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "x1", Fields: fields()})
		wantError(t, err, "not_found", `server "x1" not found`)
		_, err = app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "x1", Fields: fields(func(f *app.ServerFields) { f.Name = "" })})
		wantCode(t, err, "invalid_request")
	})

	t.Run("validation errors write and audit nothing", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		h.clean()
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.Name = "   " })})
		wantCode(t, err, "invalid_server")
		if len(h.audit.entries) != 0 || len(h.servers.writes) != 0 {
			t.Fatal("a refused update changed something")
		}
	})

	t.Run("a stale read loses: server_conflict, nothing written, no audit, no events", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		h.clean()
		// Another admin's edit commits between our read and our write.
		h.servers.beforeSave = func(slug string) {
			h.servers.beforeSave = nil
			other, _ := h.servers.FindBySlug(ctx, slug)
			if _, err := other.Update(domain.Changes{Name: domain.Some("Theirs")}, t0); err != nil {
				t.Fatal(err)
			}
			if err := h.servers.Save(ctx, other, other.PullEvents()); err != nil {
				t.Fatal(err)
			}
			h.clean()
		}
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.Name = "Mine" })})
		wantError(t, err, "server_conflict", `server "gitea" was changed meanwhile; reload it and try again`)
		if h.servers.row(t, "gitea").Name != "Theirs" || len(h.audit.entries) != 0 || len(*h.outbox) != 0 {
			t.Fatalf("a lost write left traces: row=%+v audit=%v events=%v", h.servers.row(t, "gitea"), h.audit.entries, *h.outbox)
		}
	})

	t.Run("the edit is saved at the version it was read at", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		for i := 0; i < 2; i++ {
			if _, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields()}); err != nil {
				t.Fatal(err)
			}
		}
		w := h.servers.writes
		if len(w) != 3 || w[1].ReadVersion != 0 || w[2].ReadVersion != 1 {
			t.Fatalf("writes = %+v", w)
		}
		if h.servers.row(t, "gitea").Version != 2 {
			t.Fatal("version not bumped by each save")
		}
	})
}

func TestUpdateServerAndTheSecret(t *testing.T) {
	t.Run("an auth kind change drops the secret; audit has server.secret_clear after server.update", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", ptr("old-key"), func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		v, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.Auth = authPAT })})
		if err != nil || v.SecretSet {
			t.Fatalf("view = %+v, err = %v", v, err)
		}
		if _, ok, _ := app.GetServerSecret(ctx, h.deps, "gitea"); ok {
			t.Fatal("secret still stored")
		}
		equalStrings(t, h.audit.actions(), []string{"server.update", "server.secret_clear"})
		if !h.servers.writes[0].SecretWritten {
			t.Fatal("clearing the secret must be written")
		}
		if blob, _ := json.Marshal(h.audit.entries); strings.Contains(string(blob), "old-key") {
			t.Fatal("secret leaked into the audit")
		}
	})

	t.Run("a new upstream URL drops a shared secret", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) {
			f.Auth, f.URL = authShared, "https://evil.example.com/mcp"
		})})
		if err != nil {
			t.Fatal(err)
		}
		equalStrings(t, h.audit.actions(), []string{"server.update", "server.secret_clear"})
	})

	t.Run("the secret stays while the destination stays, and is not written", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		other := authShared
		other.Header = "X-Other"
		_, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.Auth = other })})
		if err != nil {
			t.Fatal(err)
		}
		if got, _, _ := app.GetServerSecret(ctx, h.deps, "gitea"); got != "k" {
			t.Fatalf("secret = %q", got)
		}
		equalStrings(t, h.audit.actions(), []string{"server.update"})
		if h.servers.writes[0].SecretWritten {
			t.Fatal("the secret must be written only when the aggregate changed it")
		}
	})

	t.Run("an access change emits server_changed", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		h.clean()
		if _, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.Access = groups("dev") })}); err != nil {
			t.Fatal(err)
		}
		equalStrings(t, h.eventTypes(), []string{"catalog.server_changed"})
	})

	t.Run("a url-only edit of a server with no name or access change emits no event", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		h.clean()
		if _, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields(func(f *app.ServerFields) { f.URL = "https://other.example.com/mcp" })}); err != nil {
			t.Fatal(err)
		}
		if len(*h.outbox) != 0 || len(h.servers.writes[0].Events) != 0 {
			t.Fatalf("events = %v", *h.outbox)
		}
	})
}

func TestSetSecret(t *testing.T) {
	t.Run("refuses a secret for a mode that has none; clearing is fine", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "plain", nil)
		err := app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "plain", Secret: ptr("k")})
		wantError(t, err, "invalid_server", `auth mode "none" does not use a server secret`)
		if h.servers.row(t, "plain").SealedSecret != nil {
			t.Fatal("secret stored")
		}
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "plain", Secret: nil}), "ok")
	})

	t.Run("sets and clears, auditing without the value", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: ptr("topsecret")}), "ok")
		if !h.servers.writes[0].SecretWritten {
			t.Fatal("secret not written")
		}
		if got, ok, _ := app.GetServerSecret(ctx, h.deps, "gitea"); !ok || got != "topsecret" {
			t.Fatalf("secret = %q", got)
		}
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: nil}), "ok")
		if _, ok, _ := app.GetServerSecret(ctx, h.deps, "gitea"); ok {
			t.Fatal("secret not cleared")
		}
		want := []kernel.AuditEntry{
			{ActorID: ptr("admin-1"), Action: "server.secret_set", Target: ptr("gitea")},
			{ActorID: ptr("admin-1"), Action: "server.secret_clear", Target: ptr("gitea")},
		}
		if !reflect.DeepEqual(h.audit.entries, want) {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
		if blob, _ := json.Marshal(h.audit.entries); strings.Contains(string(blob), "topsecret") {
			t.Fatal("secret leaked")
		}
	})

	t.Run("clearing a server that has none still saves and audits secret_clear", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: nil}), "ok")
		equalStrings(t, h.audit.actions(), []string{"server.secret_clear"})
		if h.servers.row(t, "gitea").Version != 1 {
			t.Fatal("version not bumped")
		}
	})

	t.Run("blank, empty and too long secrets; unknown server", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: ptr("")}), "invalid_request")
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: ptr(strings.Repeat("a", 4097))}), "invalid_request")
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: ptr("   ")}), "invalid_server")
		wantError(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "zz", Secret: ptr("a")}), "not_found", `server "zz" not found`)
	})

	t.Run("a lost write is server_conflict", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
		h.clean()
		h.servers.beforeSave = func(slug string) {
			h.servers.beforeSave = nil
			h.servers.setRow(slug, func(st *domain.ServerState) { st.Version++ })
		}
		wantCode(t, app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea", Secret: ptr("k")}), "server_conflict")
		if len(h.audit.entries) != 0 {
			t.Fatal("audit written for a lost write")
		}
	})
}

func TestSetStatus(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", nil)
	h.clean()
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}), "ok")
	want := []kernel.AuditEntry{{ActorID: ptr("admin-1"), Action: "server.status", Target: ptr("gitea"), Meta: map[string]any{"status": "disabled"}}}
	if !reflect.DeepEqual(h.audit.entries, want) {
		t.Fatalf("audit = %+v", h.audit.entries)
	}
	equalStrings(t, h.eventTypes(), []string{"catalog.server_changed"})
	if len(h.servers.writes) != 1 || len(h.servers.writes[0].Events) != 1 {
		t.Fatalf("the status change and its event are one write: %+v", h.servers.writes)
	}

	// Unchanged: no save, no audit, no event.
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}), "ok")
	if len(h.audit.entries) != 1 || len(*h.outbox) != 1 || len(h.servers.writes) != 1 {
		t.Fatal("an unchanged status wrote something")
	}
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "zz", Status: domain.StatusActive}), "not_found")
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: "paused"}), "invalid_request")
}

func TestServerViewsNeverContainTheSecret(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", ptr("client-secret-value"), func(f *app.ServerFields) { f.Auth = authOAuth })
	all, err := app.ListAllServers(ctx, h.deps, admin)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := app.ListServersFor(ctx, h.deps, member())
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal([]any{all, mine})
	for _, bad := range []string{"client-secret-value", "k1:", "Y2xpZW50"} {
		if strings.Contains(string(blob), bad) {
			t.Fatalf("view leaks %q: %s", bad, blob)
		}
	}
	if !all[0].SecretSet || mine[0].AuthKind != domain.AuthOAuthUser {
		t.Fatalf("views: %+v %+v", all, mine)
	}
	// The member view has exactly these fields.
	m, _ := json.Marshal(mine[0])
	var keys map[string]any
	_ = json.Unmarshal(m, &keys)
	if len(keys) != 5 {
		t.Fatalf("member view fields = %s", m)
	}
}

func TestListServersFor(t *testing.T) {
	h := newHarness()
	for slug, access := range map[string]domain.Access{"open": accessAll, "devs": groups("dev"), "ops": groups("ops"), "off": accessAll} {
		h.mustCreate(t, slug, nil, func(f *app.ServerFields) { f.Access = access })
	}
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "off", Status: domain.StatusDisabled}), "ok")
	slugs := func(p app.Principal) []string {
		list, err := app.ListServersFor(ctx, h.deps, p)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, s := range list {
			out = append(out, s.Slug)
		}
		return out
	}
	equalStrings(t, slugs(member("dev")), []string{"devs", "open"})
	equalStrings(t, slugs(member()), []string{"open"})
	equalStrings(t, slugs(admin), []string{"devs", "open", "ops"})
	all, _ := app.ListAllServers(ctx, h.deps, admin)
	got := []string{}
	for _, s := range all {
		got = append(got, s.Slug)
	}
	equalStrings(t, got, []string{"devs", "off", "open", "ops"})
}

func TestGetServer(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", nil)
	h.mustApply(t, "gitea", snap("a"))
	v, err := app.GetServer(ctx, h.deps, admin, "gitea")
	if err != nil || v.Slug != "gitea" || v.ToolCount != 1 {
		t.Fatalf("view = %+v, err = %v", v, err)
	}
	_, err = app.GetServer(ctx, h.deps, admin, "zz")
	wantError(t, err, "not_found", "server not found")
}

func TestUpstreamQueries(t *testing.T) {
	t.Run("GetServerForUpstream has the call parameters and no secret; null when unknown or disabled", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.TimeoutMs, f.DailyLimit = ptr(9000), ptr(4) })
		got, err := app.GetServerForUpstream(ctx, h.deps, "gitea")
		if err != nil || got == nil {
			t.Fatalf("got %v, %v", got, err)
		}
		want := app.ServerCallTarget{Slug: "gitea", Name: "Gitea", URL: "https://mcp.example.com/gitea", Auth: authNone, TimeoutMs: 9000, DailyLimit: ptr(4)}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("got %+v, want %+v", *got, want)
		}
		if g, _ := app.GetServerForUpstream(ctx, h.deps, "zz"); g != nil {
			t.Fatal("unknown server has a target")
		}
		wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}), "ok")
		if g, _ := app.GetServerForUpstream(ctx, h.deps, "gitea"); g != nil {
			t.Fatal("disabled server has a target")
		}
	})

	t.Run("GetServerSecret is absent for unknown or secretless servers, and an unreadable secret is an error", func(t *testing.T) {
		h := newHarness()
		h.mustCreate(t, "gitea", nil)
		if _, ok, err := app.GetServerSecret(ctx, h.deps, "gitea"); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if _, ok, err := app.GetServerSecret(ctx, h.deps, "zz"); ok || err != nil {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		h.mustCreate(t, "shared", ptr("k"), func(f *app.ServerFields) { f.Auth = authShared })
		deps := h.deps
		deps.Box = fakeBox{id: "other"}
		if _, _, err := app.GetServerSecret(ctx, deps, "shared"); err == nil {
			t.Fatal("a secret no key opens must be an error")
		}
	})
}

func TestRepositoryErrorsPropagate(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "gitea", nil)
	boom := errBoom{}
	h.servers.failFind = func(string) error { return boom }
	if _, err := app.UpdateServer(ctx, h.deps, app.UpdateServerCommand{Principal: admin, Slug: "gitea", Fields: fields()}); err != boom {
		t.Fatalf("err = %v", err)
	}
	if err := app.SetSecret(ctx, h.deps, app.SetSecretCommand{Principal: admin, Slug: "gitea"}); err != boom {
		t.Fatalf("err = %v", err)
	}
	if err := app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}); err != boom {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.GetServerForUpstream(ctx, h.deps, "gitea"); err != boom {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.ListToolsForIndex(ctx, h.deps, "gitea"); err != boom {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.GetToolFor(ctx, h.deps, admin, "gitea.a"); err != boom {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.ListServerTools(ctx, h.deps, admin, "gitea"); err != boom {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := app.GetServerSecret(ctx, h.deps, "gitea"); err != boom {
		t.Fatalf("err = %v", err)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
