import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import {
  applyToolSnapshot,
  setServerStatus,
  setToolHidden,
  updateServer,
} from '../../src/modules/catalog/index.ts';
import { indexMissingEmbeddings, reindexAll, searchTools } from '../../src/modules/search/index.ts';
import { FakeEmbedder } from '../fakes/fake-embedder.ts';
import { startPg, truncateAll } from '../pg.ts';
import { admin, fixtureTools, makeHarness } from './harness.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
let embedder: FakeEmbedder;
let h: ReturnType<typeof makeHarness>;

beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
  embedder = new FakeEmbedder();
  // The fake embedder is a bag of tokens: a low floor keeps word overlap meaningful.
  h = makeHarness(pg.sql, embedder, 0.2);
});

const refs = async (query: string, servers: string[], limit = 10) =>
  (await searchTools(h.search, { query, limit, servers })).map((r) => r.ref);

const rows = async () =>
  pg.sql<{ ref: string; server: string; has_embedding: boolean }[]>`
    select ref, server, embedding is not null as has_embedding from search.documents order by ref`;

describe('catalog events → search.documents', () => {
  it('indexes a synchronised server and drops tools the catalog stops offering', async () => {
    await h.addServer('youtrack', 'YouTrack', 'Issue tracker', fixtureTools('youtrack'));
    await h.drain();
    expect(await rows()).toHaveLength(23);
    expect(await refs('create issue', ['youtrack'])).toContain('youtrack.create_issue');

    await setToolHidden(h.catalog, {
      principal: admin,
      ref: 'youtrack.create_issue',
      hidden: true,
    });
    await h.drain();
    expect(await refs('create issue', ['youtrack'])).not.toContain('youtrack.create_issue');
    expect(await rows()).toHaveLength(22);

    await setToolHidden(h.catalog, {
      principal: admin,
      ref: 'youtrack.create_issue',
      hidden: false,
    });
    await h.drain();
    expect(await rows()).toHaveLength(23);

    // upstream stops offering a tool → tools_removed
    await applyToolSnapshot(h.catalog, {
      server: 'youtrack',
      tools: fixtureTools('youtrack').filter((t) => t.name !== 'get_issue'),
    });
    await h.drain();
    expect((await rows()).map((r) => r.ref)).not.toContain('youtrack.get_issue');
  });

  it('removes every document of a disabled server; re-enabling indexes them again', async () => {
    await h.addServer('youtrack', 'YouTrack', 'Issue tracker', fixtureTools('youtrack'));
    await h.drain();
    await setServerStatus(h.catalog, { principal: admin, slug: 'youtrack', status: 'disabled' });
    await h.drain();
    expect(await rows()).toHaveLength(0);
    expect(await refs('create issue', ['youtrack'])).toEqual([]);
    await setServerStatus(h.catalog, { principal: admin, slug: 'youtrack', status: 'active' });
    await h.drain();
    expect(await rows()).toHaveLength(23);
  });

  it('reindexes the server on a rename (the name is part of every passage)', async () => {
    await h.addServer(
      'youtrack',
      'YouTrack',
      'Issue tracker',
      fixtureTools('youtrack').slice(0, 3),
    );
    await h.drain();
    const before = embedder.calls;
    await updateServer(h.catalog, {
      principal: admin,
      slug: 'youtrack',
      changes: { description: 'Bug tracker' },
    });
    await h.drain();
    expect(embedder.calls).toBeGreaterThan(before);
    expect(await rows()).toHaveLength(3);
  });
});

describe('server filter inside SQL', () => {
  it('returns only allowed servers even when other servers match better and limit is 1', async () => {
    await h.addServer('gitea', 'Gitea', 'Git hosting', fixtureTools('gitea'));
    await h.addServer('youtrack', 'YouTrack', 'Issue tracker', fixtureTools('youtrack'));
    await h.drain();
    const unfiltered = await refs('create issue', ['gitea', 'youtrack'], 1);
    expect(unfiltered[0]).toMatch(/^youtrack\./);
    const onlyGitea = await searchTools(h.search, {
      query: 'create issue',
      limit: 1,
      servers: ['gitea'],
    });
    expect(onlyGitea).toHaveLength(1);
    expect(onlyGitea[0]?.server).toBe('gitea');
    expect(onlyGitea[0]?.score).toBeGreaterThan(0);
    expect(onlyGitea[0]?.score).toBeLessThanOrEqual(1);
  });

  it('returns [] for no servers', async () => {
    await h.addServer('gitea', 'Gitea', 'Git hosting', fixtureTools('gitea'));
    await h.drain();
    expect(await searchTools(h.search, { query: 'issue', limit: 5, servers: [] })).toEqual([]);
  });
});

describe('names with dots and dashes', () => {
  it('finds issues.create and get-file_contents by their words', async () => {
    await h.addServer('gitea', 'Gitea', 'Git hosting', [
      { ...fixtureTools('gitea')[0]!, name: 'issues.create', description: 'x' },
      { ...fixtureTools('gitea')[1]!, name: 'get-file_contents', description: 'y' },
    ]);
    await h.drain();
    expect(await refs('issues create', ['gitea'])).toEqual(['gitea.issues.create']);
    expect(await refs('file contents', ['gitea'])).toEqual(['gitea.get-file_contents']);
  });
});

describe('embedder outages', () => {
  it('keeps text search working, and the sweep fills the embeddings in later', async () => {
    embedder.failing = true;
    await h.addServer('gitea', 'Gitea', 'Git hosting', fixtureTools('gitea').slice(0, 4));
    await expect(h.drain()).rejects.toThrow('embedder unavailable');
    const stored = await rows();
    expect(stored).toHaveLength(4);
    expect(stored.every((r) => !r.has_embedding)).toBe(true);
    const name = fixtureTools('gitea')[0]?.name ?? '';
    expect(await refs(name.replaceAll('_', ' '), ['gitea'])).toContain(`gitea.${name}`);

    embedder.failing = false;
    const swept = await indexMissingEmbeddings(h.search, { limit: 20 });
    expect(swept.indexed).toHaveLength(4);
    expect((await rows()).every((r) => r.has_embedding)).toBe(true);
  });

  it('reindexAll rebuilds the index and removes documents of vanished servers', async () => {
    await h.addServer('gitea', 'Gitea', 'Git hosting', fixtureTools('gitea').slice(0, 2));
    await h.drain();
    await pg.sql`insert into search.documents (ref, server, description, tsv)
      values ('ghost.x', 'ghost', 'd', to_tsvector('simple', 'x'))`;
    await pg.sql`delete from search.documents where ref like 'gitea.%'`;
    expect(await reindexAll(h.search)).toBe(2);
    expect((await rows()).map((r) => r.server)).toEqual(['gitea', 'gitea']);
  });

  it('skips a document marked as failed for an hour', async () => {
    embedder.failing = true;
    await h.addServer('gitea', 'Gitea', 'Git hosting', fixtureTools('gitea').slice(0, 1));
    await h.drain().catch(() => undefined);
    const [only] = await rows();
    await h.search.index.markEmbedFailed(only?.ref ?? '');
    expect(await h.search.index.withoutEmbedding(10)).toEqual([]);
    await pg.sql`update search.documents set embed_failed_at = now() - interval '2 hours'`;
    expect(await h.search.index.withoutEmbedding(10)).toHaveLength(1);
  });
});
