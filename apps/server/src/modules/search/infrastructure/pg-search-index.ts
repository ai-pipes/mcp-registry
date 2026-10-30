import type { Sql, Tx } from '../../../platform/db.ts';
import type { Candidate, IndexedDocument, SearchIndex } from '../application/ports.ts';

const toVector = (v: number[]) => `[${v.join(',')}]`;

export class PgSearchIndex implements SearchIndex {
  constructor(private readonly sql: Sql) {}

  async upsert(d: IndexedDocument): Promise<void> {
    const embedding = d.embedding ? toVector(d.embedding) : null;
    // Tool names carry dots, dashes and underscores ("gitea.issues_create"): turn them into
    // spaces so every part is a word of its own.
    await this.sql`
      insert into search.documents
        (ref, server, title, description, body, tsv, embedding, updated_at)
      values (
        ${d.ref}, ${d.server}, ${d.title}, ${d.description}, ${d.body},
        setweight(to_tsvector('simple', translate(${d.ref}, '._-/', '    ')
            || ' ' || coalesce(${d.title}::text, '')), 'A')
          || setweight(to_tsvector('russian', ${d.description}), 'B')
          || setweight(to_tsvector('english', ${d.description}), 'B')
          || setweight(to_tsvector('russian', ${d.body}), 'C')
          || setweight(to_tsvector('english', ${d.body}), 'C'),
        ${embedding}::vector,
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
        updated_at = excluded.updated_at`;
  }

  async remove(refs: string[]): Promise<void> {
    if (refs.length === 0) return;
    await this.sql`delete from search.documents where ref = any(${refs}::text[])`;
  }

  async refsOfServer(server: string): Promise<string[]> {
    const rows = await this.sql<
      { ref: string }[]
    >`select ref from search.documents where server = ${server} order by ref`;
    return rows.map((r) => r.ref);
  }

  async servers(): Promise<string[]> {
    const rows = await this.sql<
      { server: string }[]
    >`select distinct server from search.documents order by server`;
    return rows.map((r) => r.server);
  }

  async count(): Promise<number> {
    const [row] = await this.sql<{ n: number }[]>`select count(*)::int as n from search.documents`;
    return row?.n ?? 0;
  }

  async withoutEmbedding(limit: number): Promise<Candidate[]> {
    return this.sql<Candidate[]>`
      select ref, server from search.documents
      where embedding is null
        and (embed_failed_at is null or embed_failed_at < now() - interval '1 hour')
      order by embed_failed_at nulls first, ref limit ${limit}`;
  }

  async markEmbedFailed(ref: string): Promise<void> {
    await this.sql`update search.documents set embed_failed_at = now() where ref = ${ref}`;
  }

  async ftsCandidates(query: string, limit: number, servers: string[]): Promise<Candidate[]> {
    if (servers.length === 0) return [];
    return this.sql<Candidate[]>`
      with q as (
        select websearch_to_tsquery('russian', ${query})
          || websearch_to_tsquery('english', ${query})
          || websearch_to_tsquery('simple', ${query}) as q
      )
      select d.ref, d.server from search.documents d, q
      where d.tsv @@ q.q and d.server = any(${servers}::text[])
      order by ts_rank_cd(d.tsv, q.q) desc, d.ref
      limit ${limit}`;
  }

  async vectorCandidates(
    embedding: number[],
    limit: number,
    minSimilarity: number,
    servers: string[],
  ): Promise<Candidate[]> {
    if (servers.length === 0) return [];
    const q = toVector(embedding);
    // The inner query has a single sort key (the distance) so the HNSW index serves it; the
    // server filter makes iterative scan keep walking the graph until `limit` matching rows are
    // found instead of filtering the first ef_search neighbours (pgvector >= 0.8.0). τ and the
    // tie-break by ref apply outside. `<=>` is cosine distance, so similarity = 1 - distance.
    const run = (tx: Sql | Tx) => tx<Candidate[]>`
      select ref, server from (
        select ref, server, embedding <=> ${q}::vector as distance from search.documents
        where embedding is not null and server = any(${servers}::text[])
        order by embedding <=> ${q}::vector
        limit ${limit}
      ) nearest
      where 1 - distance >= ${minSimilarity}
      order by distance, ref`;
    return this.sql.begin(async (tx) => {
      await tx`set local hnsw.iterative_scan = relaxed_order`;
      return run(tx);
    });
  }
}
