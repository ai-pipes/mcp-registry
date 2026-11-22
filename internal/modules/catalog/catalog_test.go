package catalog_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
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

// EventTypes must list every event type the module emits. The source of truth is the code, read
// with go/parser (not text search): every string constant named Event* in the domain, and every
// `kernel.Event{Type: ...}` literal in the module's non-test sources, must resolve to a listed
// type. A new event constant or constructor added without extending EventTypes fails here.
func TestEventTypesListsEveryEventTheCatalogEmits(t *testing.T) {
	listed := map[string]bool{}
	for _, typ := range catalog.EventTypes() {
		if listed[typ] {
			t.Errorf("%q is listed twice", typ)
		}
		listed[typ] = true
	}

	fset := token.NewFileSet()
	consts := map[string]string{} // Event* constant name -> value
	var literals []string         // identifiers or string values used as kernel.Event{Type: x}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if !strings.HasPrefix(name.Name, "Event") || i >= len(n.Values) {
						continue
					}
					if lit, ok := n.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						consts[name.Name] = v
					}
				}
			case *ast.CompositeLit:
				sel, ok := n.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Event" {
					return true
				}
				for _, el := range n.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok || kv.Key.(*ast.Ident).Name != "Type" {
						continue
					}
					switch v := kv.Value.(type) {
					case *ast.Ident:
						literals = append(literals, v.Name)
					case *ast.BasicLit:
						s, _ := strconv.Unquote(v.Value)
						literals = append(literals, "\x00"+s)
					default:
						t.Errorf("%s: event type is not a constant or a literal", fset.Position(kv.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(consts) < 3 || len(literals) < 3 {
		t.Fatalf("the scan found %d constants and %d event literals: it is not looking at the sources", len(consts), len(literals))
	}
	for name, v := range consts {
		if !listed[v] {
			t.Errorf("constant %s (%q) is not in EventTypes", name, v)
		}
	}
	for _, l := range literals {
		v, ok := consts[l]
		if strings.HasPrefix(l, "\x00") {
			v, ok = l[1:], true
		}
		if !ok {
			t.Errorf("event literal uses %q, which is not an Event* constant", l)
		} else if !listed[v] {
			t.Errorf("event type %q is emitted but not in EventTypes", v)
		}
	}
	for typ := range listed {
		if !slices.Contains(slices.Collect(maps.Values(consts)), typ) {
			t.Errorf("EventTypes lists %q, which no Event* constant declares", typ)
		}
	}
}

func TestEventTypesReturnsACopy(t *testing.T) {
	first := catalog.EventTypes()
	first[0] = "changed"
	if catalog.EventTypes()[0] == "changed" {
		t.Fatal("EventTypes returns its own storage")
	}
}
