package search

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

// The types the composition and the other modules exchange with the search module. They are
// aliases, so a value of one is a value of the module's own type.
type (
	// Deps is what the use cases need: the index, the embedder, the catalog's tool source and plain
	// values. The composition fills it from the infrastructure.
	Deps = app.Deps
	// Embedder turns a text into a vector (Ready, EmbedPassage, EmbedQuery; the prefixes are its own).
	Embedder = app.Embedder
	// Index is the store of documents and the two candidate queries.
	Index = app.SearchIndex
	// ToolSource is the catalog as the index reads it (the catalog's Service satisfies it).
	ToolSource = app.ToolSource
	// Document is one row of the index.
	Document = app.Document
	// Logger is what the module logs to (a *slog.Logger); it logs one warning and never content.
	Logger = app.Logger
	// Candidate is a document a query or the sweep names: its ref and server (SearchIndex returns them).
	Candidate = domain.Candidate
	// ToolHit is one search result: the tool ref, its server and a score in (0, 1].
	ToolHit = domain.ToolHit
	// EventHandler handles one outbox event (the shape of the platform's outbox handler).
	EventHandler = app.EventHandler
	// SweepOptions bound one run of the sweep that fills missing vectors.
	SweepOptions = app.SweepOptions
	// SweepResult is what a sweep did (refs only).
	SweepResult = app.SweepResult
)

// Dimensions is the length of every vector (multilingual-e5-small).
const Dimensions = app.Dimensions

// MsgVectorFallback is the one record the module logs: the vector half of a search failed and the
// answer is text only.
const MsgVectorFallback = app.MsgVectorFallback

// Service is the search module's surface for the other modules, the CLI and the outbox worker. It
// holds the dependencies privately, since callers cannot name the application layer. Build it once
// with New and share it; it is safe for concurrent use as far as its ports are.
type Service struct{ d app.Deps }

// New builds the service over the module's dependencies. It panics when Index, Embedder or Tools
// is nil (a module without them would fail at the first event or search); Logger and Clock may be
// nil.
func New(d Deps) *Service {
	switch {
	case d.Index == nil:
		panic("search: Deps.Index is nil")
	case d.Embedder == nil:
		panic("search: Deps.Embedder is nil")
	case d.Tools == nil:
		panic("search: Deps.Tools is nil")
	}
	return &Service{d: d}
}

// SearchTools ranks the tools of the given servers for a query (full text and vector, fused). The
// caller decides which servers the person may use; no server means no hits. A hit's ref is
// not checked against the catalog here.
func (s *Service) SearchTools(ctx context.Context, query string, limit int, servers []string) ([]ToolHit, error) {
	return app.SearchTools(ctx, s.d, query, limit, servers)
}

// ReindexAll rebuilds the index from the catalog and drops documents of servers that are gone; it
// returns the number of rows in the index afterwards. An embedder failure does not stop the run:
// every tool still gets its text row, then the first error is returned.
func (s *Service) ReindexAll(ctx context.Context) (int, error) { return app.ReindexAll(ctx, s.d) }

// IndexMissingEmbeddings is the recovery sweep for documents stored without a vector.
func (s *Service) IndexMissingEmbeddings(ctx context.Context, opts SweepOptions) (SweepResult, error) {
	return app.IndexMissingEmbeddings(ctx, s.d, opts)
}

// EventHandlers is the table of outbox handlers for the catalog events, keyed by event type
// (exactly catalog.EventTypes()).
func (s *Service) EventHandlers() map[string]EventHandler { return app.EventHandlers(s.d) }
