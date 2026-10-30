import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vite-plus/test';
import {
  DEFAULT_MIN_SIMILARITY,
  searchTools,
  TransformersEmbedder,
} from '../../src/modules/search/index.ts';
import { startPg } from '../pg.ts';
import { fixtureTools, makeHarness } from './harness.ts';

// Gated like embedder.int.test.ts: needs the real model in $MODEL_DIR or ALLOW_MODEL_DOWNLOAD=1.
const MODEL = 'Xenova/multilingual-e5-small';
const modelDir = process.env['MODEL_DIR'];
const hasLocalModel =
  modelDir !== undefined && existsSync(join(modelDir, MODEL, 'onnx', 'model_quantized.onnx'));
const enabled = hasLocalModel || process.env['ALLOW_MODEL_DOWNLOAD'] === '1';
const SKIP_REASON = enabled
  ? ''
  : ` [skipped: no model at $MODEL_DIR/${MODEL} and ALLOW_MODEL_DOWNLOAD is not 1]`;

describe.skipIf(!enabled)(`tool search on the real model${SKIP_REASON}`, () => {
  let pg: Awaited<ReturnType<typeof startPg>>;
  let h: ReturnType<typeof makeHarness>;
  const search = (query: string, servers = ['gitea', 'youtrack'], limit = 10) =>
    searchTools(h.search, { query, limit, servers });

  beforeAll(async () => {
    pg = await startPg();
    h = makeHarness(
      pg.sql,
      new TransformersEmbedder({
        modelDir: modelDir ?? join(tmpdir(), 'mcp-registry-models'),
        allowRemote: !hasLocalModel,
      }),
      DEFAULT_MIN_SIMILARITY,
    );
    await h.addServer(
      'gitea',
      'Gitea',
      'Git hosting: repositories, pull requests',
      fixtureTools('gitea'),
    );
    await h.addServer('youtrack', 'YouTrack', 'Issue tracker', fixtureTools('youtrack'));
    await h.drain();
  }, 600_000);
  afterAll(async () => {
    await pg.stop();
  });

  it('"create an issue in the tracker" finds youtrack.create_issue first', async () => {
    const hits = await search('create an issue in the tracker');
    console.info('create an issue in the tracker', hits.slice(0, 5));
    expect(hits[0]?.ref).toBe('youtrack.create_issue');
  });

  it.each(['weather in Berlin', 'dollar exchange rate', 'borscht recipe'])(
    '"%s" finds nothing',
    async (q) => {
      const hits = await search(q);
      console.info(q, hits.slice(0, 5));
      expect(hits).toEqual([]);
    },
  );

  it('restricted to gitea, the tracker query never returns youtrack tools', async () => {
    const hits = await search('create an issue in the tracker', ['gitea'], 1);
    expect(hits.every((x) => x.server === 'gitea')).toBe(true);
  });
});
