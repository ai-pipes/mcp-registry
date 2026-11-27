create schema if not exists search;

-- One row per searchable tool (not hidden, not removed, server active), keyed by its ToolRef.
-- `embedding` is null until the embedder has run; `embed_failed_at` marks a document the
-- recovery sweep gave up on for an hour (cleared by a successful embedding).
create table search.documents (
  ref text primary key,
  server text not null,
  title text,
  description text not null,
  body text not null default '',
  tsv tsvector not null,
  embedding vector(384),
  embed_failed_at timestamptz,
  updated_at timestamptz not null default now()
);

create index documents_server_idx on search.documents (server);
create index documents_tsv_idx on search.documents using gin (tsv);
create index documents_embedding_idx on search.documents using hnsw (embedding vector_cosine_ops);
