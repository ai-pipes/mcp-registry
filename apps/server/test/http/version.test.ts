import { VersionResponse } from '@mcp/contracts';
import { Hono } from 'hono';
import { describe, expect, it } from 'vite-plus/test';
import { identityRoutes, issueLoginLink, provisionUser } from '../../src/modules/identity/index.ts';
import type { AppEnv } from '../../src/platform/http/env.ts';
import { clientIp, csrf } from '../../src/platform/http/security.ts';
import { versionRoutes } from '../../src/platform/http/version-route.ts';
import { VERSION } from '../../src/platform/version.ts';
import { makeDeps } from '../identity/fakes.ts';

const ORIGIN = 'https://mcp.example.com';

async function setup() {
  const f = makeDeps();
  const app = new Hono<AppEnv>()
    .use(clientIp({ trustProxy: false, trustProxyHops: 1, remoteAddress: () => '192.0.2.7' }))
    .use(csrf(ORIGIN))
    .route(
      '/api',
      identityRoutes(f.deps, {
        cookieSecure: true,
        oidcConfigured: false,
        logError: () => undefined,
      }),
    )
    .route('/api/version', versionRoutes(f.deps));
  const p = await provisionUser(f.deps, { sub: 'ann', name: 'ann', admin: false });
  if (!p.ok) throw p.error;
  const link = await issueLoginLink(f.deps, { sub: 'ann' });
  if (!link.ok) throw link.error;
  const res = await app.request('/api/auth/link', {
    method: 'POST',
    headers: { 'content-type': 'application/json', origin: ORIGIN },
    body: JSON.stringify({ code: link.value.code }),
  });
  const cookie = (res.headers.get('set-cookie') ?? '').split(';')[0] ?? '';
  return { app, cookie };
}

describe('GET /api/version', () => {
  it('answers the version to a signed-in person', async () => {
    const { app, cookie } = await setup();
    const res = await app.request('/api/version', { headers: { cookie } });
    expect(res.status).toBe(200);
    expect(VersionResponse.parse(await res.json())).toEqual({ version: VERSION });
  });

  it('refuses a request without a session', async () => {
    const { app } = await setup();
    expect((await app.request('/api/version')).status).toBe(401);
  });

  it('refuses a request with a bearer token', async () => {
    const { app, cookie } = await setup();
    const res = await app.request('/api/version', {
      headers: { authorization: 'Bearer sk_abcdef', cookie },
    });
    expect(res.status).toBe(401);
    expect(
      (await app.request('/api/version', { headers: { authorization: 'Bearer sk_x' } })).status,
    ).toBe(401);
  });
});
