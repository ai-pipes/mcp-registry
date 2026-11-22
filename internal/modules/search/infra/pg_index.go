package infra

import (
	"context"
	"fmt"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/app"
	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
	"github.com/ai-pipes/mcp-registry/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgIndex is app.SearchIndex on search.documents (the only table this module touches).
type PgIndex struct {
	pool *pgxpool.Pool
}

var _ app.SearchIndex = (*PgIndex)(nil)

// NewPgIndex builds the index on a pool. The similarity floor is not a property of the index: it
// arrives with every VectorCandidates call (SEARCH_MIN_SIMILARITY, held by app.Deps).
func NewPgIndex(pool *pgxpool.Pool) *PgIndex { return &PgIndex{pool: pool} }

// wrap names the operation and keeps the driver's error for errors.As; the text carries no
// parameter (no query, no document text).
func wrap(op string, err error) error { return fmt.Errorf("search index: %s: %w", op, err) }

// upsertSQL is the TypeScript statement with the parameters numbered once each (the TypeScript
// client numbers every interpolation; here the description, body, ref and title are written once
// and used by both the columns and the tsv expression). Every use is cast ::text so the parameter
// type never depends on which use the server sees first. Tool names carry dots, dashes and
// underscores ("gitea.issues_create"): translate turns them into spaces so every part is a word of
// its own. Weights: A = ref and title (simple), B = description, C = body (russian and english).
const upsertSQL = `
insert into search.documents
  (ref, server, title, description, body, tsv, embedding, updated_at)
values (
  $1::text, $2::text, $3::text, $4::text, $5::text,
  setweight(to_tsvector('simple', translate($1::text, '._-/', '    ')
      || ' ' || coalesce($3::text, '')), 'A')
    || setweight(to_tsvector('russian', $4::text), 'B')
    || setweight(to_tsvector('english', $4::text), 'B')
    || setweight(to_tsvector('russian', $5::text), 'C')
    || setweight(to_tsvector('english', $5::text), 'C'),
  $6::text::vector,
  now()
)
on conflict (ref) do update set
  server = excluded.server,
  title = excluded.title,
  description = excluded.description,
  body = excluded.body,
  tsv = excluded.tsv,
  embedding = excluded.embedding,
  embed_failed_at = case when excluded.embedding is null
    then search.documents.embed_failed_at else null end,
  updated_at = excluded.updated_at`

// Upsert writes the document by ref. The stored row takes every column from doc: a nil Embedding
// overwrites an existing vector with NULL (the TypeScript behaviour, kept on purpose) and keeps the
// embed-failed mark; a vector clears the mark.
func (p *PgIndex) Upsert(ctx context.Context, doc app.Document) error {
	var embedding *string
	if doc.Embedding != nil {
		lit, err := vectorLiteral(doc.Embedding)
		if err != nil {
			return wrap("upsert", err)
		}
		embedding = &lit
	}
	if _, err := p.pool.Exec(ctx, upsertSQL, doc.Ref, doc.Server, doc.Title, doc.Description, doc.Body, embedding); err != nil {
		return wrap("upsert", err)
	}
	return nil
}

// Remove deletes the documents of the refs; an empty list sends nothing.
func (p *PgIndex) Remove(ctx context.Context, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if _, err := p.pool.Exec(ctx, `delete from search.documents where ref = any($1::text[])`, refs); err != nil {
		return wrap("remove", err)
	}
	return nil
}

func (p *PgIndex) column(ctx context.Context, op, sql string, args ...any) ([]string, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(op, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, wrap(op, err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// RefsOfServer is the refs indexed for a server, ordered by ref (database collation).
func (p *PgIndex) RefsOfServer(ctx context.Context, server string) ([]string, error) {
	return p.column(ctx, "refs of server", `select ref from search.documents where server = $1 order by ref`, server)
}

// Servers is the servers that have at least one document, ordered by name.
func (p *PgIndex) Servers(ctx context.Context) ([]string, error) {
	return p.column(ctx, "servers", `select distinct server from search.documents order by server`)
}

// Count is the number of rows of the whole table.
func (p *PgIndex) Count(ctx context.Context) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, `select count(*)::int as n from search.documents`).Scan(&n); err != nil {
		return 0, wrap("count", err)
	}
	return n, nil
}

func candidates(rows pgx.Rows) ([]domain.Candidate, error) {
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Candidate, error) {
		var c domain.Candidate
		err := row.Scan(&c.Ref, &c.Server)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.Candidate{}
	}
	return out, nil
}

// WithoutEmbedding is up to limit documents stored without a vector, never-attempted ones first,
// then by ref; a document marked failed within the last hour is skipped.
func (p *PgIndex) WithoutEmbedding(ctx context.Context, limit int) ([]domain.Candidate, error) {
	rows, err := p.pool.Query(ctx, `
select ref, server from search.documents
where embedding is null
  and (embed_failed_at is null or embed_failed_at < now() - interval '1 hour')
order by embed_failed_at nulls first, ref limit $1`, limit)
	if err != nil {
		return nil, wrap("without embedding", err)
	}
	out, err := candidates(rows)
	if err != nil {
		return nil, wrap("without embedding", err)
	}
	return out, nil
}

// MarkEmbedFailed records that embedding ref failed, so the sweep skips it for an hour.
func (p *PgIndex) MarkEmbedFailed(ctx context.Context, ref string) error {
	if _, err := p.pool.Exec(ctx, `update search.documents set embed_failed_at = now() where ref = $1`, ref); err != nil {
		return wrap("mark embed failed", err)
	}
	return nil
}

// FTSCandidates ranks by full text: the three per-language queries (russian, english, simple) of
// websearch syntax are OR-ed, ts_rank_cd with default weights and no normalisation orders them,
// the ref breaks ties (database collation). The servers filter is in the WHERE, before the limit.
// No servers: nothing, and no query.
func (p *PgIndex) FTSCandidates(ctx context.Context, query string, limit int, servers []string) ([]domain.Candidate, error) {
	if len(servers) == 0 {
		return []domain.Candidate{}, nil
	}
	rows, err := p.pool.Query(ctx, `
with q as (
  select websearch_to_tsquery('russian', $1::text)
    || websearch_to_tsquery('english', $1::text)
    || websearch_to_tsquery('simple', $1::text) as q
)
select d.ref, d.server from search.documents d, q
where d.tsv @@ q.q and d.server = any($2::text[])
order by ts_rank_cd(d.tsv, q.q) desc, d.ref
limit $3`, query, servers, limit)
	if err != nil {
		return nil, wrap("full-text candidates", err)
	}
	out, err := candidates(rows)
	if err != nil {
		return nil, wrap("full-text candidates", err)
	}
	return out, nil
}

// vectorSQL: the inner query has a single sort key (the distance) so the HNSW index can serve it;
// the server filter makes the iterative scan (set local below) keep walking the graph until limit
// matching rows are found, instead of filtering the first ef_search neighbours. The floor and the
// ref tie-break apply outside. <=> is cosine distance, so similarity = 1 - distance. Rows without
// a vector are never candidates.
const vectorSQL = `
select ref, server from (
  select ref, server, embedding <=> $1::text::vector as distance from search.documents
  where embedding is not null and server = any($2::text[])
  order by embedding <=> $1::text::vector
  limit $3
) nearest
where 1 - distance >= $4::float8
order by distance, ref`

// VectorCandidates is the nearest documents by cosine similarity that reach minSimilarity.
// `set local hnsw.iterative_scan` (pgvector >= 0.8.0) is only effective inside the transaction it
// is issued in, so it and the query share one transaction on one connection.
func (p *PgIndex) VectorCandidates(ctx context.Context, embedding []float32, limit int, minSimilarity float64, servers []string) ([]domain.Candidate, error) {
	if len(servers) == 0 {
		return []domain.Candidate{}, nil
	}
	lit, err := vectorLiteral(embedding)
	if err != nil {
		return nil, wrap("vector candidates", err)
	}
	var out []domain.Candidate
	err = db.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `set local hnsw.iterative_scan = relaxed_order`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, vectorSQL, lit, servers, limit, minSimilarity)
		if err != nil {
			return err
		}
		out, err = candidates(rows)
		return err
	})
	if err != nil {
		return nil, wrap("vector candidates", err)
	}
	return out, nil
}
