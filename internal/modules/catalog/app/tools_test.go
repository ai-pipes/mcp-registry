package app_test

import (
	"reflect"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// seeded: gitea (open to all) with tools issues.create and list; priv (group dev) likewise.
func seeded(t *testing.T) *harness {
	t.Helper()
	h := newHarness()
	h.mustCreate(t, "gitea", nil, func(f *app.ServerFields) { f.Auth = authShared })
	h.mustCreate(t, "priv", nil, func(f *app.ServerFields) { f.Access = groups("dev") })
	h.mustApply(t, "gitea", snap("issues.create"), snap("list"))
	h.mustApply(t, "priv", snap("list"))
	h.clean()
	return h
}

func TestGetToolFor(t *testing.T) {
	t.Run("a visible tool", func(t *testing.T) {
		h := seeded(t)
		v, err := app.GetToolFor(ctx, h.deps, member(), "gitea.issues.create")
		if err != nil {
			t.Fatal(err)
		}
		if v.Server != "gitea" || v.Name != "issues.create" || v.Ref != "gitea.issues.create" || v.Title != nil ||
			v.Description != "does issues.create" || string(v.InputSchema) != `{"type":"object"}` || v.OutputSchema != nil || v.Hidden {
			t.Fatalf("view = %+v", v)
		}
	})

	t.Run("one not_found for hidden, removed, unknown, disabled and out-of-group", func(t *testing.T) {
		h := seeded(t)
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
		h.mustApply(t, "gitea", snap("list")) // issues.create goes
		get := func(ref string, p app.Principal) error {
			_, err := app.GetToolFor(ctx, h.deps, p, ref)
			return err
		}
		wantError(t, get("gitea.list", member()), "not_found", `tool "gitea.list" not found`)
		wantCode(t, get("gitea.issues.create", member()), "not_found")
		wantCode(t, get("gitea.nope", member()), "not_found")
		wantCode(t, get("nope.list", member()), "not_found")
		wantCode(t, get("priv.list", member("mcp-users")), "not_found")
		wantCode(t, get("priv.list", member("dev")), "ok")
		wantCode(t, get("priv.list", admin), "ok")
		wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "priv", Status: domain.StatusDisabled}), "ok")
		wantCode(t, get("priv.list", admin), "not_found")
		wantCode(t, get("priv.list", member("dev")), "not_found")
	})

	t.Run("a hidden tool is not found even for an admin", func(t *testing.T) {
		h := seeded(t)
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
		_, err := app.GetToolFor(ctx, h.deps, admin, "gitea.list")
		wantCode(t, err, "not_found")
	})

	t.Run("a malformed ref", func(t *testing.T) {
		h := seeded(t)
		_, err := app.GetToolFor(ctx, h.deps, member(), "gitea")
		wantError(t, err, "invalid_tool_ref", "tool name must look like <server>.<tool>")
	})
}

func TestSetToolHidden(t *testing.T) {
	t.Run("toggles, audits and writes tools_changed with the tool in one call", func(t *testing.T) {
		h := seeded(t)
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
		want := []kernel.AuditEntry{{ActorID: ptr("admin-1"), Action: "tool.hide", Target: ptr("gitea.list")}}
		if !reflect.DeepEqual(h.audit.entries, want) {
			t.Fatalf("audit = %+v", h.audit.entries)
		}
		if len(h.tools.writes) != 1 || h.tools.writes[0].Op != "save" {
			t.Fatalf("writes = %+v", h.tools.writes)
		}
		ev := h.tools.writes[0].Events
		if len(ev) != 1 || ev[0].Type != "catalog.tools_changed" ||
			!reflect.DeepEqual(ev[0].Payload, map[string]any{"server": "gitea", "refs": []string{"gitea.list"}}) {
			t.Fatalf("events = %+v", ev)
		}
		if st, _ := h.tools.row("gitea", "list"); !st.Hidden {
			t.Fatal("not hidden")
		}
		// Hiding again changes nothing.
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
		if len(*h.outbox) != 1 || len(h.audit.entries) != 1 {
			t.Fatal("an unchanged flag wrote something")
		}
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: false}), "ok")
		equalStrings(t, h.audit.actions(), []string{"tool.hide", "tool.unhide"})
	})

	t.Run("unknown tool, malformed ref and a tool upstream no longer offers", func(t *testing.T) {
		h := seeded(t)
		h.mustApply(t, "gitea", snap("list")) // issues.create removed
		h.clean()
		wantError(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.issues.create", Hidden: true}),
			"not_found", `tool "gitea.issues.create" not found`)
		wantError(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.zz", Hidden: true}),
			"not_found", `tool "gitea.zz" not found`)
		wantError(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "nope.zz", Hidden: true}),
			"not_found", `tool "nope.zz" not found`)
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "bad", Hidden: true}), "invalid_tool_ref")
		if len(h.audit.entries) != 0 || len(h.tools.writes) != 0 {
			t.Fatal("a refused hide wrote something")
		}
	})
}

func TestListServerTools(t *testing.T) {
	h := seeded(t)
	wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
	h.mustApply(t, "gitea", snap("list"), snap("new")) // issues.create removed
	got, err := app.ListServerTools(ctx, h.deps, admin, "gitea")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Name   string
		Hidden bool
	}
	var rows []row
	for _, v := range got {
		rows = append(rows, row{v.Name, v.Hidden})
	}
	if want := []row{{"list", true}, {"new", false}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %v", rows)
	}
	_, err = app.ListServerTools(ctx, h.deps, admin, "zz")
	wantError(t, err, "not_found", `server "zz" not found`)
}

func TestListCatalogFor(t *testing.T) {
	h := newHarness()
	for slug, access := range map[string]domain.Access{"open": accessAll, "closed": groups("devs"), "off": accessAll} {
		h.mustCreate(t, slug, nil, func(f *app.ServerFields) { f.Access = access })
		h.mustApply(t, slug, snap("a"), snap("b"), snap("gone"))
	}
	h.mustApply(t, "open", snap("a"), snap("b"))
	wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "open.b", Hidden: true}), "ok")
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "off", Status: domain.StatusDisabled}), "ok")

	slugs := func(p app.Principal) []string {
		r, err := app.ListCatalogFor(ctx, h.deps, p)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, s := range r {
			out = append(out, s.Slug)
		}
		return out
	}
	equalStrings(t, slugs(member("devs")), []string{"closed", "open"})
	equalStrings(t, slugs(member()), []string{"open"})
	equalStrings(t, slugs(admin), []string{"closed", "open"})

	r, _ := app.ListCatalogFor(ctx, h.deps, member("devs"))
	for _, s := range r {
		if s.Slug != "open" {
			continue
		}
		if len(s.Tools) != 1 || s.Tools[0].Ref != "open.a" || s.Tools[0].Description != "does a" || s.Tools[0].Title != nil {
			t.Fatalf("tools = %+v", s.Tools)
		}
	}
}

func TestListToolsForIndex(t *testing.T) {
	t.Run("visible tools with the server name and description", func(t *testing.T) {
		h := seeded(t)
		wantCode(t, app.SetToolHidden(ctx, h.deps, app.SetToolHiddenCommand{Principal: admin, Ref: "gitea.list", Hidden: true}), "ok")
		src, err := app.ListToolsForIndex(ctx, h.deps, "gitea")
		if err != nil || src == nil {
			t.Fatalf("src = %v, err = %v", src, err)
		}
		if src.Slug != "gitea" || src.Name != "Gitea" || src.Description != "Git hosting" || len(src.Tools) != 1 || src.Tools[0].Ref != "gitea.issues.create" ||
			src.Tools[0].Name != "issues.create" || string(src.Tools[0].InputSchema) != `{"type":"object"}` {
			t.Fatalf("src = %+v", src)
		}
	})
	t.Run("removed tools are omitted", func(t *testing.T) {
		h := seeded(t)
		h.mustApply(t, "gitea", snap("list"))
		src, _ := app.ListToolsForIndex(ctx, h.deps, "gitea")
		if len(src.Tools) != 1 || src.Tools[0].Ref != "gitea.list" {
			t.Fatalf("tools = %+v", src.Tools)
		}
	})
	t.Run("a disabled server has no tools; an unknown one is nil", func(t *testing.T) {
		h := seeded(t)
		wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "gitea", Status: domain.StatusDisabled}), "ok")
		src, _ := app.ListToolsForIndex(ctx, h.deps, "gitea")
		if src == nil || len(src.Tools) != 0 {
			t.Fatalf("src = %+v", src)
		}
		if src, err := app.ListToolsForIndex(ctx, h.deps, "ghost"); src != nil || err != nil {
			t.Fatalf("src = %v, err = %v", src, err)
		}
	})
}

func TestListServerSlugs(t *testing.T) {
	h := newHarness()
	h.mustCreate(t, "aa", nil)
	h.mustCreate(t, "bb", nil)
	wantCode(t, app.SetStatus(ctx, h.deps, app.SetStatusCommand{Principal: admin, Slug: "bb", Status: domain.StatusDisabled}), "ok")
	got, err := app.ListServerSlugs(ctx, h.deps)
	if err != nil {
		t.Fatal(err)
	}
	equalStrings(t, got, []string{"aa", "bb"})
}
