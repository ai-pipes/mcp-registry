import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import {
  applyToolSnapshot,
  type AuthMode,
  createServer,
  setServerStatus,
  setToolHidden,
  updateServer,
} from '../../src/modules/catalog/index.ts';
import { savePat } from '../../src/modules/connections/index.ts';
import { callTool, type GatewayDeps, type Principal } from '../../src/modules/gateway/index.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { loadConfig } from '../../src/platform/config.ts';
import { type FakeUpstream, PNG_BASE64, startFakeUpstream } from '../fake-upstream.ts';
import { startPg, truncateAll } from '../pg.ts';

const ADMIN: Principal = {
  userId: '33333333-3333-3333-3333-333333333333',
  role: 'admin',
  groups: [],
};
const ALICE: Principal = {
  userId: '11111111-1111-1111-1111-111111111111',
  role: 'member',
  groups: ['dev'],
  tokenId: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
};
const REF = 'fake.create_issue';

let pg: Awaited<ReturnType<typeof startPg>>;
let up: FakeUpstream;
let c: Container;
let deps: GatewayDeps;

beforeAll(async () => {
  [pg, up] = await Promise.all([startPg(), startFakeUpstream()]);
  const config = loadConfig({
    DATABASE_URL: pg.url,
    PUBLIC_URL: 'https://mcp.example.com',
    SECRETS_KEY: Buffer.alloc(32, 7).toString('base64'),
    OIDC_ISSUER: 'https://id.example.com',
    OIDC_CLIENT_ID: 'x',
    OIDC_CLIENT_SECRET: 'y',
    UPSTREAM_MAX_RESPONSE_BYTES: String(1024 * 1024),
    LOG_LEVEL: 'silent',
  });
  c = buildContainer(config, pg.sql);
  deps = c.gateway;
});
afterAll(async () => {
  await Promise.all([pg.stop(), up.close()]);
});
beforeEach(async () => {
  await truncateAll(pg.sql);
  up.reset();
  up.requests.length = 0;
});

async function seed(
  opts: { auth?: AuthMode; dailyLimit?: number; timeoutMs?: number; secret?: string } = {},
) {
  const r = await createServer(c.catalog, {
    principal: ADMIN,
    slug: 'fake',
    name: 'Fake',
    description: 'test upstream',
    url: up.url,
    auth: opts.auth ?? { kind: 'none' },
    access: { kind: 'groups', groups: ['dev'] },
    timeoutMs: opts.timeoutMs ?? 5000,
    ...(opts.dailyLimit === undefined ? {} : { dailyLimit: opts.dailyLimit }),
    ...(opts.secret === undefined ? {} : { secret: opts.secret }),
  });
  if (!r.ok) throw r.error;
  await applyToolSnapshot(c.catalog, {
    server: 'fake',
    tools: [
      {
        name: 'create_issue',
        title: null,
        description: 'Create an issue',
        inputSchema: {
          type: 'object',
          properties: { title: { type: 'string' } },
          required: ['title'],
        },
        outputSchema: null,
        annotations: {},
      },
    ],
  });
}

const call = (args: Record<string, unknown> = { title: 'Bug' }, principal = ALICE) =>
  callTool(deps, { principal, ref: REF, arguments: args });
const upstreamCalls = () => up.requests.filter((r) => r.method === 'tools/call');
const logRows = () => pg.sql`select * from gateway.calls order by id`;

const expectNotFound = async () => {
  const r = await call();
  expect(r.content).toEqual([
    {
      type: 'text',
      text: 'Tool "fake.create_issue" is not available. Use search_tools to find another.',
    },
  ]);
  expect(upstreamCalls()).toHaveLength(0);
  expect((await logRows()).map((row) => row.outcome)).toEqual(['denied:not_found']);
};

describe('callTool over Postgres, the real catalog and an MCP upstream', () => {
  it('passes the upstream result through and logs exactly the call-log columns', async () => {
    await seed({
      auth: { kind: 'shared_secret', header: 'X-API-Key', template: 'key {secret}' },
      secret: 'k-1',
    });
    const r = await call({ title: 'Bug' });
    expect(r).toStrictEqual({
      content: [{ type: 'text', text: '{"title":"Bug"}' }],
      structuredContent: { echo: { title: 'Bug' } },
    });
    expect(upstreamCalls()[0]?.headers['x-api-key']).toBe('key k-1');
    const rows = await logRows();
    expect(rows).toHaveLength(1);
    expect(Object.keys(rows[0] ?? {}).toSorted()).toEqual(
      [
        'duration_ms',
        'id',
        'outcome',
        'server',
        'started_at',
        'token_id',
        'tool',
        'user_id',
      ].toSorted(),
    );
    expect(rows[0]).toMatchObject({
      user_id: ALICE.userId,
      token_id: ALICE.tokenId,
      server: 'fake',
      tool: 'create_issue',
      outcome: 'ok',
    });
  });

  it('image content comes through unchanged', async () => {
    await seed();
    up.image();
    expect(await call()).toStrictEqual({
      content: [{ type: 'image', data: PNG_BASE64, mimeType: 'image/png' }],
    });
  });

  it('10 parallel calls with dailyLimit 3 → exactly 3 reach upstream', async () => {
    await seed({ dailyLimit: 3 });
    const results = await Promise.all(Array.from({ length: 10 }, () => call()));
    expect(upstreamCalls()).toHaveLength(3);
    expect(results.filter((r) => r.isError === true)).toHaveLength(7);
    const [q] = await pg.sql<{ count: number }[]>`select count from gateway.quota`;
    expect(q?.count).toBe(3);
    const outcomes = await pg.sql<{ outcome: string; n: number }[]>`
      select outcome, count(*)::int as n from gateway.calls group by outcome order by outcome`;
    expect(outcomes).toEqual([
      { outcome: 'denied:quota', n: 7 },
      { outcome: 'ok', n: 3 },
    ]);
  });

  it('invalid arguments: path in the text, upstream not called, quota not spent', async () => {
    await seed({ dailyLimit: 3 });
    const r = await call({ title: 5 });
    expect(r.isError).toBe(true);
    expect(r.content).toEqual([
      {
        type: 'text',
        text: 'Invalid arguments at /title: must be string. Call describe_tool("fake.create_issue") for the schema.',
      },
    ]);
    expect(upstreamCalls()).toHaveLength(0);
    expect(await pg.sql`select * from gateway.quota`).toHaveLength(0);
  });

  describe('changes between search and call (Review Focus 3)', () => {
    it('hidden by the admin', async () => {
      await seed();
      const h = await setToolHidden(c.catalog, { principal: ADMIN, ref: REF, hidden: true });
      expect(h.ok).toBe(true);
      await expectNotFound();
    });

    it('gone from upstream', async () => {
      await seed();
      await applyToolSnapshot(c.catalog, { server: 'fake', tools: [] });
      await expectNotFound();
    });

    it('server disabled', async () => {
      await seed();
      await setServerStatus(c.catalog, { principal: ADMIN, slug: 'fake', status: 'disabled' });
      await expectNotFound();
    });

    it('the person lost the group', async () => {
      await seed();
      const u = await updateServer(c.catalog, {
        principal: ADMIN,
        slug: 'fake',
        changes: { access: { kind: 'groups', groups: ['ops'] } },
      });
      expect(u.ok).toBe(true);
      await expectNotFound();
    });
  });

  it('pat_user: upstream 401 expires the connection in the database', async () => {
    await seed({
      auth: { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
    });
    const saved = await savePat(c.connections, {
      principal: { ...ALICE, name: 'Alice', via: 'session' },
      server: 'fake',
      token: 'pat-alice',
    });
    expect(saved.ok).toBe(true);
    up.status(401);
    const r = await call();
    expect(r.content).toEqual([
      {
        type: 'text',
        text: 'Fake connection expired. Ask the user to reconnect at https://mcp.example.com/connections.',
      },
    ]);
    // The 401 already answers the MCP initialize request, which carries the person's PAT.
    expect(up.requests[0]?.headers.authorization).toBe('Bearer pat-alice');
    const [conn] = await pg.sql<{ status: string }[]>`select status from connections.connections`;
    expect(conn?.status).toBe('expired');
    expect((await logRows()).map((row) => row.outcome)).toEqual(['denied:expired']);
  });

  it('pat_user without a connection: needs_connection, upstream not called', async () => {
    await seed({
      auth: { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
    });
    const r = await call();
    expect(r.content).toEqual([
      {
        type: 'text',
        text: 'Fake is not connected. Ask the user to open https://mcp.example.com/connections and connect Fake.',
      },
    ]);
    expect(upstreamCalls()).toHaveLength(0);
  });

  it('a hanging upstream: timeout text, logged timeout (Review Focus 4)', async () => {
    await seed({ timeoutMs: 1000 });
    up.slow(3000);
    const r = await call();
    expect(r).toStrictEqual({
      content: [{ type: 'text', text: 'Fake did not respond in 1 s.' }],
      isError: true,
    });
    const [row] = await logRows();
    expect(row?.outcome).toBe('timeout');
    expect(row?.duration_ms).toBeGreaterThanOrEqual(900);
  });

  it('upstream JSON-RPC errors: code and message for the agent, logged upstream_error (I1)', async () => {
    await seed();
    up.rpcError(-32602, 'Invalid params: owner is required');
    expect(await call()).toStrictEqual({
      content: [
        {
          type: 'text',
          text: 'Fake rejected the call (JSON-RPC -32602): Invalid params: owner is required',
        },
      ],
      isError: true,
    });
    up.rpcError(-32603, 'repository not found');
    const internal = await call();
    expect(internal.isError).toBe(true);
    expect(internal.content).toEqual([
      { type: 'text', text: 'Fake rejected the call (JSON-RPC -32603): repository not found' },
    ]);
    expect((await logRows()).map((row) => row.outcome)).toEqual([
      'upstream_error',
      'upstream_error',
    ]);
  });

  it('a huge answer: size text, logged transport_error (Review Focus 4)', async () => {
    await seed();
    up.huge(2 * 1024 * 1024);
    const r = await call();
    expect(r.content).toEqual([
      { type: 'text', text: 'Fake returned a response larger than 1 MiB.' },
    ]);
    expect((await logRows()).map((row) => row.outcome)).toEqual(['transport_error']);
  });
});

const at = (iso: string) => new Date(iso);
const entry = (
  startedAt: Date,
  extra: Partial<Parameters<GatewayDeps['callLog']['record']>[0]> = {},
) => ({
  userId: ALICE.userId,
  tokenId: null,
  server: 'fake',
  tool: 'create_issue',
  startedAt,
  durationMs: 12,
  outcome: 'ok' as const,
  ...extra,
});

describe('PgCallLog', () => {
  it('lists newest first with filters and paging', async () => {
    await deps.callLog.record(entry(at('2026-10-01T00:00:00Z')));
    await deps.callLog.record(entry(at('2026-10-02T00:00:00Z'), { outcome: 'timeout' }));
    await deps.callLog.record(entry(at('2026-10-03T00:00:00Z'), { server: 'other' }));
    await deps.callLog.record(
      entry(at('2026-10-04T00:00:00Z'), { userId: ADMIN.userId, tokenId: ALICE.tokenId ?? null }),
    );
    const all = await deps.callLog.list({ limit: 10 });
    expect(all.map((e) => e.startedAt.toISOString().slice(0, 10))).toEqual([
      '2026-10-04',
      '2026-10-03',
      '2026-10-02',
      '2026-10-01',
    ]);
    expect(all[0]).toMatchObject({ userId: ADMIN.userId, tokenId: ALICE.tokenId, durationMs: 12 });
    expect((await deps.callLog.list({ limit: 10, server: 'other' })).length).toBe(1);
    expect((await deps.callLog.list({ limit: 10, outcome: 'timeout' })).length).toBe(1);
    expect((await deps.callLog.list({ limit: 10, userId: ADMIN.userId })).length).toBe(1);
    const page = await deps.callLog.list({ limit: 2, before: at('2026-10-03T00:00:00Z') });
    expect(page.map((e) => e.outcome)).toEqual(['timeout', 'ok']);
  });

  it('deleteOlderThan removes only older entries and returns the count', async () => {
    await deps.callLog.record(entry(at('2026-07-01T00:00:00Z')));
    await deps.callLog.record(entry(at('2026-07-02T00:00:00Z')));
    await deps.callLog.record(entry(at('2026-10-01T00:00:00Z')));
    expect(await deps.callLog.deleteOlderThan(at('2026-08-01T00:00:00Z'))).toBe(2);
    expect(await deps.callLog.list({ limit: 10 })).toHaveLength(1);
  });
});

describe('PgQuota.deleteDaysBefore', () => {
  it('deletes counters of earlier days only', async () => {
    const days = ['2026-10-27', '2026-10-28', '2026-10-29', '2026-10-30'];
    await Promise.all(
      days.map((day) =>
        deps.quota.tryConsume({ userId: ALICE.userId, server: 'fake', day, limit: 5 }),
      ),
    );
    expect(await deps.quota.deleteDaysBefore('2026-10-29')).toBe(2);
    const left = await pg.sql<{ day: string }[]>`
      select to_char(day, 'YYYY-MM-DD') as day from gateway.quota order by day`;
    expect(left.map((r) => r.day)).toEqual(['2026-10-29', '2026-10-30']);
  });
});
