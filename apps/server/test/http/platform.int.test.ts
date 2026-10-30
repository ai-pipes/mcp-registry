import { randomBytes } from 'node:crypto';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Hono } from 'hono';
import { afterAll, beforeAll, describe, expect, it } from 'vite-plus/test';
import { DEFAULT_MIN_SIMILARITY } from '../../src/modules/search/index.ts';
import type { Config } from '../../src/platform/config.ts';
import { buildContainer } from '../../src/platform/container.ts';
import { createDb } from '../../src/platform/db.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import type { AppEnv } from '../../src/platform/http/env.ts';
import { createLogger } from '../../src/platform/logger.ts';
import { startPg } from '../pg.ts';

const ORIGIN = 'http://mcp.test';
const CSP =
  "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
  "frame-ancestors 'none'";

let pg: Awaited<ReturnType<typeof startPg>>;
let webRoot: string;
let logLines: string[] = [];

function configFor(): Config {
  return {
    port: 3000,
    databaseUrl: 'postgres://unused',
    publicUrl: `${ORIGIN}/`,
    allowedHosts: [],
    trustProxy: false,
    trustProxyHops: 1,
    cookieSecure: true,
    secretsKey: new Uint8Array(randomBytes(32)),
    oidc: {
      issuer: 'https://id.example.com',
      clientId: 'id',
      clientSecret: 'secret',
      loginGroup: 'mcp-users',
      adminGroup: 'mcp-admins',
      scopes: 'openid profile email groups',
      groupsClaim: 'groups',
    },
    syncIntervalHours: 6,
    callLogRetentionDays: 90,
    upstreamMaxResponseBytes: 20 * 1024 * 1024,
    mcpMaxBodyBytes: 10 * 1024 * 1024,
    embedder: 'local',
    modelDir: '/nonexistent',
    searchMinSimilarity: DEFAULT_MIN_SIMILARITY,
    logLevel: 'info',
  };
}

function makeApp(sql = pg.sql): Hono<AppEnv> {
  const logger = createLogger('info', { write: (line: string) => logLines.push(line) });
  const container = buildContainer(configFor(), sql, { logger });
  return createHttpApp(container, { remoteAddress: () => '192.0.2.1', webRoot }).app;
}

beforeAll(async () => {
  pg = await startPg();
  webRoot = await mkdtemp(join(tmpdir(), 'web-root-'));
  await mkdir(join(webRoot, 'assets'));
  await writeFile(join(webRoot, 'index.html'), '<!doctype html><div id="root"></div>');
  await writeFile(join(webRoot, 'assets', 'app-abc123.js'), 'export {}');
});
afterAll(async () => {
  await pg.stop();
  await rm(webRoot, { recursive: true, force: true });
});

describe('platform HTTP app', () => {
  it('/healthz answers ok while the database is up', async () => {
    const res = await makeApp().request('/healthz');
    expect(res.status).toBe(200);
    expect(await res.text()).toBe('ok');
  });

  it('/healthz answers 503 when the database is down', async () => {
    const dead = createDb('postgres://nobody@127.0.0.1:1/none');
    const res = await makeApp(dead).request('/healthz');
    expect(res.status).toBe(503);
    expect(await res.text()).toBe('fail');
    await dead.end({ timeout: 0 });
  });

  it('sends the strict CSP and a request id on every response', async () => {
    const res = await makeApp().request('/healthz');
    expect(res.headers.get('content-security-policy')).toBe(CSP);
    expect(res.headers.get('x-request-id')).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('serves the SPA shell for browser navigations and static assets with long caching', async () => {
    const app = makeApp();
    const page = await app.request('/servers/gitea', { headers: { accept: 'text/html' } });
    expect(page.status).toBe(200);
    expect(await page.text()).toContain('<div id="root"></div>');
    expect(page.headers.get('cache-control')).toBe('no-cache');
    const asset = await app.request('/assets/app-abc123.js');
    expect(asset.status).toBe(200);
    expect(asset.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
  });

  it('does not hand the SPA shell to /api, /mcp or /healthz paths', async () => {
    const app = makeApp();
    const api = await app.request('/api/nothing', { headers: { accept: 'text/html' } });
    expect(api.status).toBe(404);
    expect(await api.json()).toMatchObject({ error: { code: 'not_found' } });
    // /mcp is the MCP endpoint: without a Bearer token it answers 401, never the SPA shell.
    const mcp = await app.request('/mcp', { headers: { accept: 'text/html', host: 'localhost' } });
    expect(mcp.status).toBe(401);
    expect(mcp.headers.get('content-type')).not.toContain('text/html');
  });

  it('rejects a mutating /api request without the right Origin (CSRF), even with a Bearer token', async () => {
    const app = makeApp();
    const bare = await app.request('/api/x', { method: 'POST' });
    expect(bare.status).toBe(403);
    expect(await bare.json()).toMatchObject({ error: { code: 'csrf' } });
    const bearer = await app.request('/api/x', {
      method: 'POST',
      headers: { authorization: 'Bearer sk_anything' },
    });
    expect(bearer.status).toBe(403);
    const ok = await app.request('/api/x', { method: 'POST', headers: { origin: ORIGIN } });
    expect(ok.status).toBe(404);
  });

  it('caps request bodies at 1 MiB', async () => {
    const res = await makeApp().request('/api/x', {
      method: 'POST',
      headers: { origin: ORIGIN, 'content-length': String(2 * 1024 * 1024) },
      body: 'x',
    });
    expect(res.status).toBe(413);
    expect(await res.json()).toMatchObject({ error: { code: 'limit_exceeded' } });
  });

  it('logs method, path and status, never headers', async () => {
    logLines = [];
    await makeApp().request('/healthz', { headers: { authorization: 'Bearer sk_secret-token' } });
    const text = logLines.join('');
    expect(text).toContain('"path":"/healthz"');
    expect(text).not.toContain('sk_secret-token');
  });
});
