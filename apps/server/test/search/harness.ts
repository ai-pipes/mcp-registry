import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { z } from 'zod';
import {
  applyToolSnapshot,
  type CatalogDeps,
  createPgCatalog,
  createServer,
  type Principal,
  type ToolSnapshot,
} from '../../src/modules/catalog/index.ts';
import {
  createCatalogToolReader,
  createPgSearch,
  DEFAULT_MIN_SIMILARITY,
  type Embedder,
  type SearchDeps,
  searchEventHandlers,
} from '../../src/modules/search/index.ts';
import { PgAuditLog } from '../../src/platform/audit.ts';
import type { Sql } from '../../src/platform/db.ts';
import { claimOutbox, markFailed, markProcessed } from '../../src/platform/outbox.ts';
import { AesGcmSecretBox } from '../../src/platform/crypto/secret-box.ts';
import { systemClock } from '../../src/shared-kernel/index.ts';

export const admin: Principal = {
  userId: '00000000-0000-4000-8000-000000000001',
  role: 'admin',
  groups: [],
};

const fixtureSchema = z.object({
  tools: z.array(
    z.object({
      name: z.string(),
      description: z.string().optional(),
      inputSchema: z.record(z.string(), z.unknown()).optional(),
      annotations: z.record(z.string(), z.unknown()).optional(),
    }),
  ),
});

/** A real `tools/list` response recorded in the spike (test/fixtures/tools/<name>.json). */
export function fixtureTools(name: string): ToolSnapshot[] {
  const path = fileURLToPath(new URL(`../fixtures/tools/${name}.json`, import.meta.url));
  const raw: unknown = JSON.parse(readFileSync(path, 'utf8'));
  return fixtureSchema.parse(raw).tools.map((t) => ({
    name: t.name,
    title: null,
    description: t.description ?? '',
    inputSchema: { type: 'object', ...t.inputSchema },
    outputSchema: null,
    // ToolAnnotations are the four boolean hints; other keys (e.g. title) are dropped.
    annotations: Object.fromEntries(
      Object.entries(t.annotations ?? {}).filter(([, v]) => typeof v === 'boolean'),
    ),
  }));
}

/** Catalog and search over one Postgres; `drain` delivers the outbox to the search handlers. */
export function makeHarness(sql: Sql, embedder: Embedder, minSimilarity = DEFAULT_MIN_SIMILARITY) {
  const catalog: CatalogDeps = {
    ...createPgCatalog(sql),
    secretBox: new AesGcmSecretBox(new Uint8Array(32).fill(7)),
    clock: systemClock,
    audit: new PgAuditLog(sql),
  };
  const search: SearchDeps = {
    ...createPgSearch(sql),
    tools: createCatalogToolReader(catalog),
    embedder,
    minSimilarity,
    logger: { warn: () => undefined },
  };
  const handlers = searchEventHandlers(search);

  /** Delivers pending outbox events to the search handlers; a failing handler is rethrown after markFailed. */
  async function drain(): Promise<void> {
    const rows = await claimOutbox(sql, 50);
    if (rows.length === 0) return;
    await rows.reduce(async (prev, row) => {
      await prev;
      try {
        await handlers[row.type]?.(row.payload);
        await markProcessed(sql, row.id);
      } catch (error) {
        await markFailed(sql, row.id, error);
        throw error;
      }
    }, Promise.resolve());
    await drain();
  }

  async function addServer(
    slug: string,
    name: string,
    description: string,
    tools: ToolSnapshot[],
  ): Promise<void> {
    const created = await createServer(catalog, {
      principal: admin,
      slug,
      name,
      description,
      url: `https://mcp.example.com/${slug}`,
      auth: { kind: 'none' },
      access: { kind: 'all' },
    });
    if (!created.ok) throw created.error;
    await applyToolSnapshot(catalog, { server: slug, tools });
  }

  return { catalog, search, drain, addServer };
}
