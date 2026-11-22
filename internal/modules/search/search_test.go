package search_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/search"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

// The ports fail every call they do not implement (the nil embedded interface panics).
type index struct{ app.SearchIndex }

func (index) Count(context.Context) (int, error) { return 7, nil }

func (index) Servers(context.Context) ([]string, error) { return nil, nil }

func (index) FTSCandidates(context.Context, string, int, []string) ([]domain.Candidate, error) {
	return []domain.Candidate{{Ref: "gitea.search", Server: "gitea"}}, nil
}

type embedder struct{ app.Embedder }

func (embedder) Ready(context.Context) bool { return false }

type tools struct{ app.ToolSource }

func (tools) ListServerSlugs(context.Context) ([]string, error) { return nil, nil }

func deps() search.Deps {
	return search.Deps{Index: index{}, Embedder: embedder{}, Tools: tools{}, MinSimilarity: 0.83}
}

func TestNewRefusesAnIncompleteDeps(t *testing.T) {
	for name, mutate := range map[string]func(*search.Deps){
		"index":    func(d *search.Deps) { d.Index = nil },
		"embedder": func(d *search.Deps) { d.Embedder = nil },
		"tools":    func(d *search.Deps) { d.Tools = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := deps()
			mutate(&d)
			defer func() {
				if recover() == nil {
					t.Fatalf("New accepted Deps without %s", name)
				}
			}()
			search.New(d)
		})
	}
	search.New(deps()) // a nil Logger and Clock are fine
}

// The table is checked against catalog.EventTypes() in search/app (this package may not import the
// catalog, its own test included); here only that the service hands it out whole.
func TestServiceHandlersAreTheThreeCatalogEvents(t *testing.T) {
	got := []string{}
	for typ, h := range search.New(deps()).EventHandlers() {
		if h == nil {
			t.Errorf("handler of %s is nil", typ)
		}
		got = append(got, typ)
	}
	want := []string{"catalog.server_changed", "catalog.tools_changed", "catalog.tools_removed"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("handlers for %v, want %v", got, want)
	}
}

func TestServiceSearchReachesTheUseCase(t *testing.T) {
	// The embedder is not ready: text only, so the one full-text candidate comes back.
	hits, err := search.New(deps()).SearchTools(context.Background(), "search", 5, []string{"gitea"})
	if err != nil || len(hits) != 1 || hits[0].Ref != "gitea.search" || hits[0].Server != "gitea" || hits[0].Score <= 0 {
		t.Fatalf("hits = %+v, err = %v", hits, err)
	}
}

func TestServiceReindexAllReturnsTheCountOfTheIndex(t *testing.T) {
	n, err := search.New(deps()).ReindexAll(context.Background())
	if err != nil || n != 7 {
		t.Fatalf("ReindexAll = %d, %v", n, err)
	}
}

func TestServiceSweepDoesNothingUntilTheEmbedderIsReady(t *testing.T) {
	res, err := search.New(deps()).IndexMissingEmbeddings(context.Background(), search.SweepOptions{Limit: 20})
	if err != nil || len(res.Indexed) != 0 || len(res.Failed) != 0 || res.StoppedEarly != "" {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
}
