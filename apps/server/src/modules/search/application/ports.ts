export interface Embedder {
  readonly dimensions: 384;
  embedPassages(texts: string[]): Promise<number[][]>;
  embedQuery(text: string): Promise<number[]>;
  /** True once the model is usable; must not block on a model download. */
  ready(): Promise<boolean>;
}

/** A visible tool of a server, as the catalog hands it over. */
export type ToolSource = {
  ref: string;
  name: string;
  title: string | null;
  description: string;
  inputSchema: Record<string, unknown>;
};

export type ServerToolsSource = {
  slug: string;
  name: string;
  description: string;
  /** Only searchable tools: not hidden, not removed; empty while the server is disabled. */
  tools: ToolSource[];
};

/** Read access to the catalog, implemented over its public API. */
export interface ToolReader {
  /** Null when the server does not exist (any more). */
  forServer(slug: string): Promise<ServerToolsSource | null>;
  serverSlugs(): Promise<string[]>;
}

export type IndexedDocument = {
  ref: string;
  server: string;
  title: string | null;
  description: string;
  /** Parameter names and descriptions: full-text only. */
  body: string;
  embedding: number[] | null;
};

/** A search candidate: the ref plus its server, so a hit needs no second query. */
export type Candidate = { ref: string; server: string };

export interface SearchIndex {
  upsert(doc: IndexedDocument): Promise<void>;
  remove(refs: string[]): Promise<void>;
  refsOfServer(server: string): Promise<string[]>;
  /** Servers that have at least one document. */
  servers(): Promise<string[]>;
  count(): Promise<number>;
  /**
   * Up to `limit` documents stored without an embedding, never-attempted first (then by ref),
   * skipping those whose embedding failed within the last hour.
   */
  withoutEmbedding(limit: number): Promise<Candidate[]>;
  /** Records that embedding `ref` failed, so the sweep skips it for a while. A successful upsert with an embedding clears the mark. */
  markEmbedFailed(ref: string): Promise<void>;
  /** Refs ranked by full-text relevance, best first; only documents of `servers`, filtered before `limit`. */
  ftsCandidates(query: string, limit: number, servers: string[]): Promise<Candidate[]>;
  /**
   * Documents whose cosine similarity to `embedding` is at least `minSimilarity`, nearest first;
   * only documents of `servers`, filtered before `limit`. Without the floor every query would
   * return the whole index in some order.
   */
  vectorCandidates(
    embedding: number[],
    limit: number,
    minSimilarity: number,
    servers: string[],
  ): Promise<Candidate[]>;
}

/** Structured logger (pino satisfies it). Fields never carry query text or document content. */
interface SearchLogger {
  warn(fields: Record<string, unknown>, message: string): void;
}

export type SearchDeps = {
  logger: SearchLogger;
  index: SearchIndex;
  embedder: Embedder;
  tools: ToolReader;
  /** Cosine-similarity floor τ for vector candidates (SEARCH_MIN_SIMILARITY). */
  minSimilarity: number;
};
