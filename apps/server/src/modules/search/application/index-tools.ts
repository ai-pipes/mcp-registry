import { buildPassage, schemaText } from '../domain/search-document.ts';
import type { IndexedDocument, SearchDeps, ToolSource } from './ports.ts';

/**
 * Brings the search documents of one server in line with the catalog.
 * `refs` given → only those refs: each is (re)indexed if the catalog still offers it (not hidden,
 * not removed, server active), otherwise removed. Without `refs` → the whole server: every offered
 * tool is (re)indexed and documents of tools it no longer offers are removed. An unknown or
 * disabled server has no tools, so its documents disappear.
 * If the embedder fails, every document is still written (text search works, `embedding = null`)
 * and the error is rethrown so the outbox retries and the sweep fills the embeddings in later.
 */
export async function indexTools(
  deps: Pick<SearchDeps, 'index' | 'embedder' | 'tools'>,
  cmd: { server: string; refs?: string[] },
): Promise<void> {
  const source = await deps.tools.forServer(cmd.server);
  const offered = source?.tools ?? [];
  const wanted = cmd.refs ? offered.filter((t) => cmd.refs?.includes(t.ref)) : offered;
  const keep = new Set(wanted.map((t) => t.ref));
  const candidates = cmd.refs ?? (await deps.index.refsOfServer(cmd.server));
  await deps.index.remove(candidates.filter((ref) => !keep.has(ref)));

  // One passage per embedder call, sequentially. The q8 model quantises activations dynamically
  // with a scale shared by the whole batch (padding takes part), so a document's vector shifts by
  // up to ~0.005 with its batch mates; the gap between neighbouring hits is about 0.002. Sorting
  // or bucketing batches by length does not remove this. After the first failure the remaining
  // tools are written without an embedding.
  let failure: unknown = null;
  const docs = await wanted.reduce<Promise<IndexedDocument[]>>(async (prev, t) => {
    const done = await prev;
    let embedding: number[] | null = null;
    if (failure === null) {
      try {
        [embedding = null] = await deps.embedder.embedPassages([
          passageOf(source?.name ?? '', source?.description ?? '', t),
        ]);
      } catch (error) {
        failure = error;
      }
    }
    return [...done, toDocument(cmd.server, t, embedding)];
  }, Promise.resolve([]));
  await Promise.all(docs.map((d) => deps.index.upsert(d)));
  if (failure !== null) throw failure;
}

const passageOf = (serverName: string, serverDescription: string, t: ToolSource): string =>
  buildPassage({
    serverName,
    serverDescription,
    toolName: t.name,
    title: t.title,
    description: t.description,
  });

const toDocument = (
  server: string,
  t: ToolSource,
  embedding: number[] | null,
): IndexedDocument => ({
  ref: t.ref,
  server,
  title: t.title,
  description: t.description,
  body: schemaText(t.inputSchema),
  embedding,
});

/** Deletes the documents of tools upstream no longer offers. */
export async function removeTools(
  deps: Pick<SearchDeps, 'index'>,
  cmd: { refs: string[] },
): Promise<void> {
  await deps.index.remove(cmd.refs);
}
