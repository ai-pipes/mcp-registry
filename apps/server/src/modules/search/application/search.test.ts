import { beforeEach, describe, expect, it } from 'vite-plus/test';
import { FakeEmbedder } from '../../../../test/fakes/fake-embedder.ts';
import { searchEventHandlers } from './event-handlers.ts';
import { indexMissingEmbeddings } from './index-missing-embeddings.ts';
import { indexTools, removeTools } from './index-tools.ts';
import type {
  Candidate,
  IndexedDocument,
  SearchDeps,
  SearchIndex,
  ServerToolsSource,
  ToolReader,
  ToolSource,
} from './ports.ts';
import { reindexAll } from './reindex-all.ts';
import { searchTools } from './search-tools.ts';

class MemoryIndex implements SearchIndex {
  readonly docs = new Map<string, IndexedDocument>();
  readonly failed = new Set<string>();
  /** `servers` argument of every candidate query. */
  readonly serverArgs: { fts: string[][]; vector: string[][] } = { fts: [], vector: [] };

  async upsert(doc: IndexedDocument): Promise<void> {
    this.docs.set(doc.ref, doc);
    if (doc.embedding) this.failed.delete(doc.ref);
  }
  async remove(refs: string[]): Promise<void> {
    for (const r of refs) this.docs.delete(r);
  }
  async refsOfServer(server: string): Promise<string[]> {
    return [...this.docs.values()].filter((d) => d.server === server).map((d) => d.ref);
  }
  async servers(): Promise<string[]> {
    return [...new Set([...this.docs.values()].map((d) => d.server))];
  }
  async count(): Promise<number> {
    return this.docs.size;
  }
  async withoutEmbedding(limit: number): Promise<Candidate[]> {
    return [...this.docs.values()]
      .filter((d) => d.embedding === null && !this.failed.has(d.ref))
      .toSorted((a, b) => (a.ref < b.ref ? -1 : 1))
      .slice(0, limit)
      .map((d) => ({ ref: d.ref, server: d.server }));
  }
  async markEmbedFailed(ref: string): Promise<void> {
    this.failed.add(ref);
  }
  private cand = (d: IndexedDocument): Candidate => ({ ref: d.ref, server: d.server });
  async ftsCandidates(query: string, limit: number, servers: string[]): Promise<Candidate[]> {
    this.serverArgs.fts.push(servers);
    const words = query.toLowerCase().split(/\s+/);
    return [...this.docs.values()]
      .filter((d) => servers.includes(d.server))
      .filter((d) =>
        words.every((w) => `${d.ref} ${d.title} ${d.description} ${d.body}`.includes(w)),
      )
      .slice(0, limit)
      .map(this.cand);
  }
  async vectorCandidates(
    embedding: number[],
    limit: number,
    minSimilarity: number,
    servers: string[],
  ): Promise<Candidate[]> {
    this.serverArgs.vector.push(servers);
    const dot = (v: number[]) => v.reduce((s, x, i) => s + x * (embedding[i] ?? 0), 0);
    return [...this.docs.values()]
      .filter((d) => servers.includes(d.server))
      .filter((d) => d.embedding !== null && dot(d.embedding) >= minSimilarity)
      .toSorted((a, b) => dot(b.embedding ?? []) - dot(a.embedding ?? []))
      .slice(0, limit)
      .map(this.cand);
  }
}

class MemoryReader implements ToolReader {
  readonly servers = new Map<string, ServerToolsSource>();
  async forServer(slug: string): Promise<ServerToolsSource | null> {
    return this.servers.get(slug) ?? null;
  }
  async serverSlugs(): Promise<string[]> {
    return [...this.servers.keys()];
  }
  /** (Re)defines a server; `tools` maps tool name → description. */
  put(slug: string, tools: Record<string, string>, over: Partial<ServerToolsSource> = {}): void {
    this.servers.set(slug, {
      slug,
      name: slug,
      description: `${slug} server`,
      tools: Object.entries(tools).map(([name, description]) => tool(slug, name, description)),
      ...over,
    });
  }
  hide(slug: string, name: string): void {
    const s = this.servers.get(slug);
    if (s) s.tools = s.tools.filter((t) => t.name !== name);
  }
}

const tool = (server: string, name: string, description: string): ToolSource => ({
  ref: `${server}.${name}`,
  name,
  title: null,
  description,
  inputSchema: { type: 'object', properties: { query: { description: 'search words' } } },
});

const doc = (ref: string, server = ref.split('.')[0] ?? ''): IndexedDocument => ({
  ref,
  server,
  title: null,
  description: '',
  body: '',
  embedding: null,
});

let index: MemoryIndex;
let reader: MemoryReader;
let embedder: FakeEmbedder;
let deps: SearchDeps;
let warnings: { fields: Record<string, unknown>; message: string }[];

beforeEach(() => {
  index = new MemoryIndex();
  reader = new MemoryReader();
  embedder = new FakeEmbedder();
  warnings = [];
  deps = {
    index,
    tools: reader,
    embedder,
    minSimilarity: 0.5,
    logger: { warn: (fields, message) => warnings.push({ fields, message }) },
  };
});

describe('indexTools', () => {
  it('indexes every visible tool of a server with body and embedding', async () => {
    reader.put('gitea', { issue: 'create issue', pr: 'open pull request' });
    await indexTools(deps, { server: 'gitea' });
    expect([...index.docs.keys()].toSorted()).toEqual(['gitea.issue', 'gitea.pr']);
    expect(index.docs.get('gitea.issue')).toMatchObject({
      server: 'gitea',
      description: 'create issue',
      body: 'query search words',
    });
    expect(index.docs.get('gitea.issue')?.embedding).toHaveLength(384);
  });

  it('indexes only the given refs and leaves the rest of the server alone', async () => {
    reader.put('gitea', { issue: 'create issue', pr: 'open pull request' });
    await indexTools(deps, { server: 'gitea', refs: ['gitea.pr'] });
    expect([...index.docs.keys()]).toEqual(['gitea.pr']);
  });

  it('removes a listed ref the reader no longer offers (hidden tool)', async () => {
    reader.put('gitea', { issue: 'create issue', pr: 'open pull request' });
    await indexTools(deps, { server: 'gitea' });
    reader.hide('gitea', 'pr');
    await indexTools(deps, { server: 'gitea', refs: ['gitea.pr'] });
    expect([...index.docs.keys()]).toEqual(['gitea.issue']);
  });

  it('drops documents of a disabled, emptied or deleted server', async () => {
    reader.put('gitea', { issue: 'create issue' });
    await indexTools(deps, { server: 'gitea' });
    reader.put('gitea', {}); // disabled: no tools
    await indexTools(deps, { server: 'gitea' });
    expect(index.docs.size).toBe(0);
    await index.upsert(doc('ghost.x'));
    await indexTools(deps, { server: 'ghost' }); // unknown server
    expect(index.docs.size).toBe(0);
  });

  it('writes the full-text rows without embeddings and rethrows when the embedder fails', async () => {
    reader.put('gitea', { issue: 'create issue' });
    embedder.failing = true;
    await expect(indexTools(deps, { server: 'gitea' })).rejects.toThrow('embedder unavailable');
    expect(index.docs.get('gitea.issue')?.embedding).toBeNull();
  });

  it('embeds one passage per call, so a vector does not depend on its neighbours', async () => {
    reader.put('big', Object.fromEntries(Array.from({ length: 40 }, (_, i) => [`t${i}`, 'x'])));
    await indexTools(deps, { server: 'big' });
    expect(index.docs.size).toBe(40);
    expect(embedder.calls).toBe(40);
  });
});

describe('removeTools', () => {
  it('deletes the documents of the refs', async () => {
    await index.upsert(doc('gitea.a'));
    await index.upsert(doc('gitea.b'));
    await removeTools(deps, { refs: ['gitea.a'] });
    expect([...index.docs.keys()]).toEqual(['gitea.b']);
  });
});

describe('searchTools', () => {
  beforeEach(async () => {
    reader.put('gitea', { pdf: 'extract text from pdf', review: 'code review before merge' });
    reader.put('youtrack', { borscht: 'cooking recipe', pdf: 'pdf report export' });
    await reindexAll(deps);
  });

  it('returns [] for no servers or a blank query without touching the index or embedder', async () => {
    const before = embedder.calls;
    expect(await searchTools(deps, { query: 'pdf', limit: 10, servers: [] })).toEqual([]);
    expect(await searchTools(deps, { query: '   ', limit: 10, servers: ['gitea'] })).toEqual([]);
    expect(await searchTools(deps, { query: 'pdf', limit: 0, servers: ['gitea'] })).toEqual([]);
    expect(embedder.calls).toBe(before);
    expect(index.serverArgs).toEqual({ fts: [], vector: [] });
  });

  it('returns [] when nothing matches the text and no vector clears the floor', async () => {
    expect(
      await searchTools(deps, { query: 'weather tomorrow', limit: 10, servers: ['gitea'] }),
    ).toEqual([]);
  });

  it('scores a text-only hit 0.5 and a first-in-both hit 1', async () => {
    expect(await searchTools(deps, { query: 'merge', limit: 10, servers: ['gitea'] })).toEqual([
      { ref: 'gitea.review', server: 'gitea', score: 0.5 },
    ]);
    // The server name and description are part of the passage, so a lower floor keeps it in the vector list.
    const low = { ...deps, minSimilarity: 0.3 };
    const [first, ...rest] = await searchTools(low, {
      query: 'cooking recipe',
      limit: 10,
      servers: ['youtrack'],
    });
    expect(first).toEqual({ ref: 'youtrack.borscht', server: 'youtrack', score: 1 });
    expect(rest.every((r) => r.score > 0 && r.score <= 1)).toBe(true);
  });

  it('gives an FTS-only hit the same score whether or not the embedder is ready', async () => {
    const ready = await searchTools(deps, { query: 'merge', limit: 10, servers: ['gitea'] });
    embedder.ready = async () => false;
    expect(await searchTools(deps, { query: 'merge', limit: 10, servers: ['gitea'] })).toEqual(
      ready,
    );
  });

  it('falls back to text-only when the embedder fails, and skips overlong queries', async () => {
    embedder.failing = true;
    expect(warnings).toEqual([]);
    expect(
      (await searchTools(deps, { query: 'merge', limit: 10, servers: ['gitea'] })).map(
        (r) => r.ref,
      ),
    ).toEqual(['gitea.review']);
    // Exactly one warning, with the error label only: no query text anywhere in it.
    expect(warnings).toHaveLength(1);
    expect(warnings[0]?.fields).toEqual({ err: expect.stringContaining('Error') });
    expect(JSON.stringify(warnings)).not.toContain('merge');
    embedder.failing = false;
    const before = embedder.calls;
    expect(
      await searchTools(deps, { query: 'z'.repeat(2001), limit: 10, servers: ['gitea'] }),
    ).toEqual([]);
    expect(embedder.calls).toBe(before);
  });

  it('never returns documents of servers outside the filter, even when more relevant, with limit 1', async () => {
    // youtrack is indexed first, so it outranks gitea for "pdf"; the filter must still win.
    reader.servers.clear();
    index.docs.clear();
    reader.put('youtrack', { pdf: 'pdf report export' });
    reader.put('gitea', { pdf: 'extract text from pdf' });
    await reindexAll(deps);
    const both = await searchTools(deps, {
      query: 'pdf',
      limit: 1,
      servers: ['gitea', 'youtrack'],
    });
    expect(both.map((r) => r.ref)).toEqual(['youtrack.pdf']);
    const res = await searchTools(deps, { query: 'pdf', limit: 1, servers: ['gitea'] });
    expect(res.map((r) => r.ref)).toEqual(['gitea.pdf']);
    expect(index.serverArgs.fts.at(-1)).toEqual(['gitea']);
    expect(index.serverArgs.vector.at(-1)).toEqual(['gitea']);
  });

  it('respects limit', async () => {
    expect(
      await searchTools(deps, { query: 'pdf', limit: 1, servers: ['gitea', 'youtrack'] }),
    ).toHaveLength(1);
  });

  it('keeps tool names with dots intact', async () => {
    reader.put('gitea', { 'issues.create': 'create an issue' });
    await indexTools(deps, { server: 'gitea' });
    const res = await searchTools(deps, { query: 'create an issue', limit: 5, servers: ['gitea'] });
    expect(res[0]).toMatchObject({ ref: 'gitea.issues.create', server: 'gitea' });
  });
});

describe('reindexAll', () => {
  it('indexes every server, cleans up vanished ones and returns the document count', async () => {
    reader.put('a', { x: 'one' });
    reader.put('b', { y: 'two' }, { tools: [] }); // disabled
    await index.upsert(doc('stale.t'));
    await index.upsert(doc('b.y'));
    expect(await reindexAll(deps)).toBe(1);
    expect([...index.docs.keys()]).toEqual(['a.x']);
  });

  it('writes every text row before rethrowing an embedder failure', async () => {
    reader.put('a', { x: 'one' });
    reader.put('b', { y: 'two' });
    embedder.failing = true;
    await expect(reindexAll(deps)).rejects.toThrow('embedder unavailable');
    expect([...index.docs.keys()].toSorted()).toEqual(['a.x', 'b.y']);
  });
});

describe('searchEventHandlers', () => {
  it('subscribes to the three catalog events', () => {
    expect(Object.keys(searchEventHandlers(deps)).toSorted()).toEqual([
      'catalog.server_changed',
      'catalog.tools_changed',
      'catalog.tools_removed',
    ]);
  });

  it('tools_changed indexes the refs and drops a tool the admin hid', async () => {
    const h = searchEventHandlers(deps);
    reader.put('gitea', { issue: 'create issue', pr: 'open pull request' });
    await h['catalog.tools_changed']?.({ server: 'gitea', refs: ['gitea.issue', 'gitea.pr'] });
    expect(index.docs.size).toBe(2);
    reader.hide('gitea', 'pr');
    await h['catalog.tools_changed']?.({ server: 'gitea', refs: ['gitea.pr'] });
    expect([...index.docs.keys()]).toEqual(['gitea.issue']);
  });

  it('tools_removed deletes the documents', async () => {
    const h = searchEventHandlers(deps);
    await index.upsert(doc('gitea.a'));
    await index.upsert(doc('gitea.b'));
    await h['catalog.tools_removed']?.({ server: 'gitea', refs: ['gitea.a'] });
    expect([...index.docs.keys()]).toEqual(['gitea.b']);
  });

  it('server_changed reindexes all tools of the server and removes them when it is disabled', async () => {
    const h = searchEventHandlers(deps);
    reader.put('gitea', { issue: 'create issue', pr: 'open pull request' }, { name: 'Git' });
    await h['catalog.server_changed']?.({ server: 'gitea' });
    expect(index.docs.size).toBe(2);
    const before = embedder.calls;
    reader.put('gitea', {});
    await h['catalog.server_changed']?.({ server: 'gitea' });
    expect(index.docs.size).toBe(0);
    expect(embedder.calls).toBe(before);
  });

  it('rejects malformed payloads', async () => {
    const h = searchEventHandlers(deps);
    await expect(h['catalog.server_changed']?.({})).rejects.toThrow('server');
    await expect(h['catalog.tools_changed']?.({ server: 'g', refs: 'x' })).rejects.toThrow('refs');
    await expect(h['catalog.tools_removed']?.({ server: 'g', refs: [1] })).rejects.toThrow('refs');
  });
});

const missing = () => [...index.docs.values()].filter((d) => d.embedding === null).length;

describe('indexMissingEmbeddings', () => {
  beforeEach(async () => {
    reader.put('s', { a: 'tool a', b: 'tool b', c: 'tool c' });
    embedder.failing = true;
    await reindexAll(deps).catch(() => undefined);
    embedder.failing = false;
  });

  it('re-indexes documents without an embedding, at most `limit` per run', async () => {
    expect(missing()).toBe(3);
    expect((await indexMissingEmbeddings(deps, { limit: 2 })).indexed).toEqual(['s.a', 's.b']);
    expect(missing()).toBe(1);
    expect((await indexMissingEmbeddings(deps, { limit: 2 })).indexed).toEqual(['s.c']);
    expect(await indexMissingEmbeddings(deps, { limit: 2 })).toEqual({
      indexed: [],
      failed: [],
      stoppedEarly: null,
    });
  });

  it('does nothing while the embedder is not ready', async () => {
    embedder.ready = async () => false;
    const before = embedder.calls;
    expect((await indexMissingEmbeddings(deps, { limit: 20 })).indexed).toEqual([]);
    expect(embedder.calls).toBe(before);
  });

  it('marks only an isolated poison document and then skips it', async () => {
    const real = embedder.embedPassages.bind(embedder);
    embedder.embedPassages = async (texts) => {
      if (texts.some((t) => t.includes('tool a'))) throw new Error('poison');
      return real(texts);
    };
    expect(await indexMissingEmbeddings(deps, { limit: 20 })).toEqual({
      indexed: ['s.b', 's.c'],
      failed: ['s.a'],
      stoppedEarly: null,
    });
    expect([...index.failed]).toEqual(['s.a']);
    const before = embedder.calls;
    expect((await indexMissingEmbeddings(deps, { limit: 20 })).indexed).toEqual([]);
    expect(embedder.calls).toBe(before);
  });

  it('treats 3 consecutive failures as an outage: stops, marks nothing', async () => {
    embedder.failing = true;
    const before = embedder.calls;
    expect(await indexMissingEmbeddings(deps, { limit: 20 })).toEqual({
      indexed: [],
      failed: ['s.a', 's.b', 's.c'],
      stoppedEarly: 'outage',
    });
    expect(embedder.calls).toBe(before + 4); // 3 documents + 1 probe
    expect(index.failed.size).toBe(0);
  });

  it('marks a poison streak when the probe succeeds and carries on', async () => {
    reader.put('s', { p1: 'poison', p2: 'poison', p3: 'poison', zok: 'fine' });
    index.docs.clear();
    embedder.failing = true;
    await reindexAll(deps).catch(() => undefined);
    embedder.failing = false;
    const real = embedder.embedPassages.bind(embedder);
    embedder.embedPassages = async (texts) => {
      if (texts.some((t) => t.includes('poison'))) throw new Error('poison');
      return real(texts);
    };
    const res = await indexMissingEmbeddings(deps, { limit: 20 });
    expect(res.indexed).toEqual(['s.zok']);
    expect(res.failed).toEqual(['s.p1', 's.p2', 's.p3']);
    expect([...index.failed].toSorted()).toEqual(['s.p1', 's.p2', 's.p3']);
  });

  it('stops when the wall-clock budget is spent', async () => {
    let t = 0;
    const res = await indexMissingEmbeddings(deps, {
      limit: 20,
      budgetMs: 10,
      now: () => (t += 6),
    });
    expect(res.stoppedEarly).toBe('budget');
    expect(res.indexed).toEqual(['s.a']);
  });
});
