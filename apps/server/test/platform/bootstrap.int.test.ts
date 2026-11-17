import { randomBytes } from 'node:crypto';
import { Writable } from 'node:stream';
import { type ServerType, serve } from '@hono/node-server';
import { Client, StreamableHTTPClientTransport } from '@modelcontextprotocol/client';
import { z } from 'zod';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import {
  authenticateSession,
  completeLinkLogin,
  createToken,
  provisionUser,
} from '../../src/modules/identity/index.ts';
import { announceFirstStart } from '../../src/platform/bootstrap.ts';
import type { Config } from '../../src/platform/config.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import { createLogger } from '../../src/platform/logger.ts';
import { FakeEmbedder } from '../fakes/fake-embedder.ts';
import { startPg, truncateAll } from '../pg.ts';

const LogRecord = z.record(z.string(), z.unknown());
const PUBLIC_URL = 'https://mcp.example.com';

let pg: Awaited<ReturnType<typeof startPg>>;

function configFor(oidc: boolean): Config {
  return {
    port: 3000,
    databaseUrl: 'postgres://unused',
    databasePassword: undefined,
    publicUrl: PUBLIC_URL,
    allowedHosts: [],
    trustProxy: false,
    trustProxyHops: 1,
    cookieSecure: true,
    secretsKey: new Uint8Array(randomBytes(32)),
    oidc: {
      client: oidc
        ? { issuer: 'https://id.example.com', clientId: 'id', clientSecret: 'secret' }
        : null,
      loginGroup: 'mcp-users',
      adminGroup: 'mcp-admins',
      scopes: 'openid profile email groups',
      groupsClaim: 'groups',
    },
    syncIntervalHours: 6,
    callLogRetentionDays: 90,
    upstreamMaxResponseBytes: 1024 * 1024,
    mcpMaxBodyBytes: 10 * 1024 * 1024,
    embedder: 'local',
    modelDir: '/nonexistent',
    searchMinSimilarity: 0.83,
    logLevel: 'silent',
  };
}

const containerFor = (config: Config): Container =>
  buildContainer(config, pg.sql, { embedder: new FakeEmbedder(), logger: createLogger('silent') });

/** Runs the announcement and returns the log records it produced. */
async function start(
  config: Config,
): Promise<{ c: Container; records: Record<string, unknown>[] }> {
  const lines: string[] = [];
  const sink = new Writable({
    write(chunk: Buffer, _enc, done) {
      lines.push(chunk.toString());
      done();
    },
  });
  const c = containerFor(config);
  await announceFirstStart(c, config, createLogger('info', sink));
  const records = lines.map((l) => LogRecord.parse(JSON.parse(l)));
  return { c, records };
}

const codeOf = (record: Record<string, unknown> | undefined): string => {
  const url = String(record?.url);
  return url.slice(url.indexOf('#link=') + '#link='.length);
};

beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
});

describe('announceFirstStart', () => {
  it('prints one sign-in link on an empty database without OIDC', async () => {
    const { c, records } = await start(configFor(false));
    expect(records).toHaveLength(1);
    const [record] = records;
    expect(record?.level).toBe(40);
    expect(record?.msg).toBe(
      'first start: open this one-time link within 15 minutes to sign in as admin',
    );
    expect(String(record?.url)).toMatch(new RegExp(`^${PUBLIC_URL}/login#link=ll_`));
    const done = await completeLinkLogin(c.identity, { code: codeOf(record), ip: '203.0.113.9' });
    if (!done.ok) throw done.error;
    const principal = await authenticateSession(c.identity, { sessionId: done.value.sessionId });
    expect(principal?.role).toBe('admin');
  });

  it('prints a new link on a restart before the admin has signed in', async () => {
    const first = await start(configFor(false));
    const second = await start(configFor(false));
    expect(first.records).toHaveLength(1);
    expect(second.records).toHaveLength(1);
    expect(codeOf(second.records[0])).not.toBe(codeOf(first.records[0]));
    const stale = await completeLinkLogin(second.c.identity, {
      code: codeOf(first.records[0]),
      ip: '203.0.113.9',
    });
    expect(!stale.ok && stale.error.code).toBe('invalid_link');
    const done = await completeLinkLogin(second.c.identity, {
      code: codeOf(second.records[0]),
      ip: '203.0.113.9',
    });
    expect(done.ok).toBe(true);
  });

  it('prints nothing once the admin has signed in', async () => {
    const first = await start(configFor(false));
    const done = await completeLinkLogin(first.c.identity, {
      code: codeOf(first.records[0]),
      ip: '203.0.113.9',
    });
    expect(done.ok).toBe(true);
    const second = await start(configFor(false));
    expect(second.records).toEqual([]);
  });

  it('prints nothing when other people exist', async () => {
    const c = containerFor(configFor(false));
    const made = await provisionUser(c.identity, { sub: 'bob', name: 'Bob', admin: false });
    if (!made.ok) throw made.error;
    const { records } = await start(configFor(false));
    expect(records).toEqual([]);
  });

  it('logs and continues when the announcement fails', async () => {
    const config = configFor(false);
    const real = containerFor(config);
    const failing: Container = {
      ...real,
      identity: {
        ...real.identity,
        users: {
          ...real.identity.users,
          list: () => Promise.reject(new Error('secret-db-detail')),
        },
      },
    };
    const lines: string[] = [];
    const sink = new Writable({
      write(chunk: Buffer, _enc, done) {
        lines.push(chunk.toString());
        done();
      },
    });
    await expect(
      announceFirstStart(failing, config, createLogger('info', sink)),
    ).resolves.toBeUndefined();
    const records = lines.map((l) => LogRecord.parse(JSON.parse(l)));
    expect(records).toHaveLength(1);
    expect(records[0]?.level).toBe(50);
    expect(lines.join('')).not.toMatch(/secret-db-detail|#link=|ll_/);
  });

  it('prints nothing and creates nobody when OIDC is configured', async () => {
    const { records } = await start(configFor(true));
    expect(records).toEqual([]);
    const [row] = await pg.sql<{ n: number }[]>`select count(*)::int as n from identity.users`;
    expect(row?.n).toBe(0);
  });
});

describe('a server without OIDC', () => {
  let server: ServerType;
  let http: { close: () => Promise<void> };
  let client: Client | undefined;

  afterAll(async () => {
    await client?.close();
    await http.close();
    await new Promise<void>((resolve) => {
      server.close(() => resolve());
    });
  });

  it('serves /mcp with a token when OIDC is not configured', async () => {
    const c = containerFor(configFor(false));
    const built = createHttpApp(c, { remoteAddress: () => '192.0.2.1', webRoot: '/nonexistent' });
    http = built;
    await new Promise<void>((resolve) => {
      server = serve({ fetch: built.app.fetch, port: 0, hostname: '127.0.0.1' }, () => resolve());
    });
    const address = server.address();
    if (address === null || typeof address === 'string') throw new Error('no TCP address');
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const made = await provisionUser(c.identity, { sub: 'bob', name: 'Bob', admin: false });
    if (!made.ok) throw made.error;
    const { token } = await createToken(c.identity, {
      principal: {
        userId: made.value.id,
        name: 'Bob',
        role: 'member',
        groups: [],
        via: 'session',
      },
      name: 'cli',
    });
    client = new Client({ name: 'test-agent', version: '1.0.0' });
    await client.connect(
      new StreamableHTTPClientTransport(new URL(`${baseUrl}/mcp`), {
        requestInit: { headers: { authorization: `Bearer ${token}` } },
      }),
    );
    const tools = await client.listTools();
    expect(tools.tools.map((t) => t.name)).toContain('search_tools');

    const methods = await fetch(`${baseUrl}/api/auth/methods`);
    expect(await methods.json()).toEqual({ oidc: false });
  });
});
