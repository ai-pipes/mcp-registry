package app

import (
	"context"

	"github.com/ai-pipes/mcp-registry/internal/kernel"
	"github.com/ai-pipes/mcp-registry/internal/modules/catalog"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

// Dimensions is the length of every vector (multilingual-e5-small); the column is vector(384).
const Dimensions = 384

// Embedder turns text into vectors. The prefixes belong to the implementation, as in the
// TypeScript embedder: EmbedPassage embeds "passage: <text>" and EmbedQuery embeds
// "query: <text>"; the use cases pass the bare text. One text per call, never a batch: the q8
// model scales its activations per run, so a vector must not depend on its neighbours.
// Implementations are safe for concurrent use (a search and an indexing handler can call at once).
type Embedder interface {
	// Ready reports whether the model is usable now. It never blocks on a load; as a side effect
	// it starts the load when none is running (and retries a failed one), and a load failure is
	// not reported here: Ready just keeps answering false.
	Ready(ctx context.Context) bool
	// EmbedPassage waits for the model to load. It returns exactly Dimensions numbers or an error.
	EmbedPassage(ctx context.Context, text string) ([]float32, error)
	// EmbedQuery is EmbedPassage with the query prefix.
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// ToolSource is the catalog as the index needs it: the catalog's root Service satisfies it.
type ToolSource interface {
	// ListToolsForIndex is nil, nil for an unknown server; a disabled server has no tools.
	ListToolsForIndex(ctx context.Context, slug string) (*catalog.ServerIndexSource, error)
	// ListServerSlugs is every server, disabled ones included.
	ListServerSlugs(ctx context.Context) ([]string, error)
}

// Document is one row of search.documents as the use cases write it.
type Document struct {
	Ref         string
	Server      string
	Title       *string
	Description string
	// Body is the parameter names and descriptions: full text only.
	Body string
	// Embedding is nil for a document stored without a vector (SQL NULL), else Dimensions long.
	Embedding []float32
}

// SearchIndex is the store of documents (schema search.*).
type SearchIndex interface {
	// Upsert writes the document by ref: the stored row takes every column from doc, so a nil
	// Embedding clears an existing vector (the TypeScript behaviour, kept on purpose). A non-nil
	// Embedding also clears the embed-failed mark.
	Upsert(ctx context.Context, doc Document) error
	// Remove deletes the documents of the refs; unknown refs are ignored, an empty list is a no-op.
	Remove(ctx context.Context, refs []string) error
	// RefsOfServer is the refs indexed for a server, ordered by ref.
	RefsOfServer(ctx context.Context, server string) ([]string, error)
	// Servers is the servers that have at least one document, ordered by server (that order
	// decides which error ReindexAll reports first).
	Servers(ctx context.Context) ([]string, error)
	// Count is the number of rows of the whole table.
	Count(ctx context.Context) (int, error)
	// WithoutEmbedding is up to limit documents stored without a vector, in the sweep's order
	// (never attempted first, then by ref), skipping those marked failed within the last hour.
	WithoutEmbedding(ctx context.Context, limit int) ([]domain.Candidate, error)
	// MarkEmbedFailed records that embedding ref failed, so the sweep skips it for a while.
	MarkEmbedFailed(ctx context.Context, ref string) error
	// FTSCandidates is up to limit refs ranked by full-text relevance, best first, among the
	// documents of servers only (filtered before the limit).
	FTSCandidates(ctx context.Context, query string, limit int, servers []string) ([]domain.Candidate, error)
	// VectorCandidates is up to limit documents whose cosine similarity to embedding is at least
	// minSimilarity, nearest first, among the documents of servers only (filtered before the limit).
	VectorCandidates(ctx context.Context, embedding []float32, limit int, minSimilarity float64, servers []string) ([]domain.Candidate, error)
}

// Logger receives the warnings the module absorbs. Fields never carry query text or document
// content. *slog.Logger satisfies it.
type Logger interface {
	Warn(msg string, args ...any)
}

// Deps is everything the use cases need: ports and plain values.
type Deps struct {
	Index    SearchIndex
	Embedder Embedder
	Tools    ToolSource
	// Logger may be nil.
	Logger Logger
	// Clock may be nil (the wall clock); the sweep's time budget reads it.
	Clock kernel.Clock
	// MinSimilarity is the cosine floor of vector candidates (SEARCH_MIN_SIMILARITY).
	MinSimilarity float64
}

func (d Deps) clock() kernel.Clock {
	if d.Clock == nil {
		return kernel.SystemClock{}
	}
	return d.Clock
}
