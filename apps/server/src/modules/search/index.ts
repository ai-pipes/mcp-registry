// Public API of the search module: the only file other modules may import.
import type { Sql } from '../../platform/db.ts';
import type { SearchDeps } from './application/ports.ts';
import { PgSearchIndex } from './infrastructure/pg-search-index.ts';

export { searchEventHandlers, type EventHandler } from './application/event-handlers.ts';
export { indexMissingEmbeddings } from './application/index-missing-embeddings.ts';
export { indexTools } from './application/index-tools.ts';
export type { Embedder, SearchDeps, ToolReader } from './application/ports.ts';
export { reindexAll } from './application/reindex-all.ts';
export { DEFAULT_MIN_SIMILARITY, searchTools, type ToolHit } from './application/search-tools.ts';
export { createCatalogToolReader } from './infrastructure/catalog-tool-reader.ts';
export { OpenAiCompatibleEmbedder } from './infrastructure/openai-compatible-embedder.ts';
export { TransformersEmbedder } from './infrastructure/transformers-embedder.ts';

export function createPgSearch(sql: Sql): Pick<SearchDeps, 'index'> {
  return { index: new PgSearchIndex(sql) };
}
