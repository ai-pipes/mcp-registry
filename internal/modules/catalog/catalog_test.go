package catalog_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog/domain"
)

// servers serves a fixed list and fails every other call of the port (the nil embedded interface).
type servers struct {
	app.ServerRepository
	rows []domain.ServerState
}

func (s servers) FindBySlug(_ context.Context, slug string) (*domain.Server, error) {
	for _, r := range s.rows {
		if r.Slug == slug {
			return domain.RehydrateServer(r), nil
		}
	}
	return nil, nil
}

func (s servers) List(context.Context) ([]app.ServerListing, error) {
	out := []app.ServerListing{}
	for _, r := range s.rows {
		out = append(out, app.ServerListing{Server: domain.RehydrateServer(r), ToolCount: 2})
	}
	return out, nil
}

type tools struct{ app.ToolRepository }

func (tools) ListByServer(context.Context, string) ([]*domain.Tool, error) {
	return []*domain.Tool{}, nil
}

func state(slug string, status domain.ServerStatus, sealed []byte) domain.ServerState {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return domain.ServerState{
		Slug: slug, Name: "Gitea", Description: "Git", URL: "https://mcp.example.com/g", SealedSecret: sealed,
		Auth:   domain.AuthMode{Kind: domain.AuthSharedSecret, Header: "X-Key", Template: "{secret}"},
		Access: domain.Access{Kind: domain.AccessAll}, TimeoutMs: 1000, Status: status, CreatedAt: now, UpdatedAt: now,
	}
}

type box struct{}

func (box) Seal(p string) ([]byte, error) { return []byte("s:" + p), nil }
func (box) Open(b []byte) (string, error) { return string(b[2:]), nil }

var member = catalog.Principal{UserID: "u", Role: "member", Groups: []string{}}

func TestServiceAnswersWhatOtherModulesAsk(t *testing.T) {
	svc := catalog.New(catalog.Deps{
		Servers: servers{rows: []domain.ServerState{state("alpha", domain.StatusActive, []byte("s:key")), state("beta", domain.StatusDisabled, nil)}},
		Tools:   tools{}, Box: box{},
	})
	ctx := context.Background()

	list, err := svc.ListServersFor(ctx, member)
	if err != nil || len(list) != 1 || list[0].Slug != "alpha" || list[0].AuthKind != catalog.AuthSharedSecret || list[0].ToolCount != 2 {
		t.Fatalf("ListServersFor: %+v %v", list, err)
	}
	target, err := svc.GetServerForUpstream(ctx, "alpha")
	if err != nil || target == nil || target.URL != "https://mcp.example.com/g" || target.Auth.Kind != catalog.AuthSharedSecret {
		t.Fatalf("GetServerForUpstream: %+v %v", target, err)
	}
	if target, err := svc.GetServerForUpstream(ctx, "beta"); err != nil || target != nil {
		t.Fatalf("a disabled server has no call target: %+v %v", target, err)
	}
	secret, ok, err := svc.GetServerSecret(ctx, "alpha")
	if err != nil || !ok || secret != "key" {
		t.Fatalf("GetServerSecret: %q %v %v", secret, ok, err)
	}
	if _, ok, err := svc.GetServerSecret(ctx, "beta"); ok || err != nil {
		t.Fatalf("no secret: %v %v", ok, err)
	}
	slugs, err := svc.ListServerSlugs(ctx)
	if err != nil || !reflect.DeepEqual(slugs, []string{"alpha", "beta"}) {
		t.Fatalf("ListServerSlugs: %v %v", slugs, err)
	}
	src, err := svc.ListToolsForIndex(ctx, "alpha")
	if err != nil || src == nil || src.Slug != "alpha" || len(src.Tools) != 0 {
		t.Fatalf("ListToolsForIndex: %+v %v", src, err)
	}
	if src, err := svc.ListToolsForIndex(ctx, "nope"); err != nil || src != nil {
		t.Fatalf("unknown: %+v %v", src, err)
	}
	entries, err := svc.ListCatalogFor(ctx, member)
	if err != nil || len(entries) != 1 || entries[0].Slug != "alpha" || entries[0].Tools == nil {
		t.Fatalf("ListCatalogFor: %+v %v", entries, err)
	}
	if _, err := svc.GetToolFor(ctx, member, "alpha"); err == nil {
		t.Fatal("a ref without a tool must be refused")
	} else if de, _ := kernel.AsDomainError(err); de == nil || de.Code != catalog.CodeInvalidToolRef {
		t.Fatalf("GetToolFor: %v", err)
	}
}

func TestHelpersOfTheDomainAreReachable(t *testing.T) {
	mode := catalog.AuthMode{Kind: catalog.AuthSharedSecret, Header: "Authorization", Template: "Bearer {secret}"}
	if got := catalog.HeaderFor(mode, "tok")["Authorization"]; got != "Bearer tok" {
		t.Fatalf("HeaderFor: %q", got)
	}
	server, tool, err := catalog.ParseToolRef("gitea.issue.list")
	if err != nil || server != "gitea" || tool != "issue.list" {
		t.Fatalf("ParseToolRef: %q %q %v", server, tool, err)
	}
	if catalog.FormatToolRef("gitea", "issue.list") != "gitea.issue.list" {
		t.Fatal("FormatToolRef")
	}
	if len(catalog.ValidateServerInput(catalog.ServerInput{Slug: "", Fields: catalog.ServerFields{}})) == 0 {
		t.Fatal("an empty input is not valid")
	}
}

func TestEventPayloadsAsTheOutboxDeliversThem(t *testing.T) {
	if catalog.EventToolsChanged != "catalog.tools_changed" || catalog.EventToolsRemoved != "catalog.tools_removed" || catalog.EventServerChanged != "catalog.server_changed" {
		t.Fatal("event types")
	}
	// What the domain writes, and what comes back after a JSON round trip.
	written := domain.ToolsChangedEvent("gitea", []string{"gitea.a", "gitea.b"})
	for name, payload := range map[string]map[string]any{
		"as written":  written.Payload,
		"after JSON":  {"server": "gitea", "refs": []any{"gitea.a", "gitea.b"}},
		"empty refs":  {"server": "gitea", "refs": []any{}},
		"no refs key": {"server": "gitea"},
		"bad ref":     {"server": "gitea", "refs": []any{1}},
		"bad refs":    {"server": "gitea", "refs": "gitea.a"},
		"no server":   {"refs": []any{}},
	} {
		ev, ok := catalog.ParseToolsEvent(payload)
		wantOK := name == "as written" || name == "after JSON" || name == "empty refs"
		if ok != wantOK {
			t.Errorf("%s: ok=%v", name, ok)
			continue
		}
		if ok && (ev.Server != "gitea" || ev.Refs == nil) {
			t.Errorf("%s: %+v", name, ev)
		}
		if name != "empty refs" && ok && !reflect.DeepEqual(ev.Refs, []string{"gitea.a", "gitea.b"}) {
			t.Errorf("%s: refs %v", name, ev.Refs)
		}
	}
	if s, ok := catalog.ParseServerEvent(domain.ServerChangedEvent("gitea").Payload); !ok || s != "gitea" {
		t.Fatal("ParseServerEvent")
	}
	if _, ok := catalog.ParseServerEvent(map[string]any{"server": 3}); ok {
		t.Fatal("ParseServerEvent accepted a number")
	}
}

// TestServiceTypesAreTheModulesOwn: the aliases let a value cross without a conversion.
func TestServiceTypesAreTheModulesOwn(t *testing.T) {
	for name, same := range map[string]bool{
		"ServerView":      reflect.TypeFor[catalog.ServerView]() == reflect.TypeFor[app.ServerView](),
		"Deps":            reflect.TypeFor[catalog.Deps]() == reflect.TypeFor[app.Deps](),
		"Entry":           reflect.TypeFor[catalog.Entry]() == reflect.TypeFor[app.CatalogEntry](),
		"SyncCredentials": reflect.TypeFor[catalog.SyncCredentials]() == reflect.TypeFor[app.SyncCredentials](),
	} {
		if !same {
			t.Errorf("%s is not the module's own type", name)
		}
	}
}

func TestSourcesAndAccessKindsAreReExported(t *testing.T) {
	if catalog.AccessAll != "all" || catalog.AccessGroups != "groups" ||
		catalog.SourceNone != "none" || catalog.SourceServerSecret != "server_secret" || catalog.SourcePersonal != "personal" {
		t.Fatal("constants")
	}
	if a := (catalog.Access{Kind: catalog.AccessAll}); a.Kind != "all" {
		t.Fatal("Access")
	}
	if src := catalog.SourcePersonal; src != "personal" {
		t.Fatal("CredentialSource")
	}
}

func TestServiceCommandsReachTheUseCases(t *testing.T) {
	svc := catalog.New(catalog.Deps{
		Servers: servers{rows: []domain.ServerState{state("beta", domain.StatusDisabled, nil)}},
		Tools:   tools{}, Box: box{}, Clock: kernel.SystemClock{},
	})
	ctx := context.Background()
	if out, err := svc.SyncAllServers(ctx); err != nil || len(out) != 0 {
		t.Fatalf("SyncAllServers over a disabled server: %+v %v", out, err)
	}
	if _, err := svc.SyncServer(ctx, catalog.SyncCommand{Slug: "beta"}); err == nil {
		t.Fatal("a disabled server cannot be synchronised")
	} else if de, _ := kernel.AsDomainError(err); de == nil || de.Code != catalog.CodeNotFound {
		t.Fatalf("SyncServer: %v", err)
	}
	sum, err := svc.ResealServerSecrets(ctx, box{}, box{})
	if err != nil || sum.Resealed != 0 || sum.Current != 0 || len(sum.Failed) != 0 {
		t.Fatalf("ResealServerSecrets: %+v %v", sum, err)
	}
	if _, err := svc.RegisterServer(ctx, catalog.CreateServerCommand{Principal: member}); err == nil {
		t.Fatal("a member cannot register a server")
	} else if de, _ := kernel.AsDomainError(err); de == nil || de.Code != catalog.CodeForbidden {
		t.Fatalf("RegisterServer: %v", err)
	}
}
