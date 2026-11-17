import { Hono } from 'hono';
import { describe, expect, it } from 'vite-plus/test';
import { issueLoginLink, identityRoutes, provisionUser } from '../../src/modules/identity/index.ts';
import { domainErrorResponse } from '../../src/platform/http/errors.ts';
import type { AppEnv } from '../../src/platform/http/env.ts';
import { clientIp, csrf } from '../../src/platform/http/security.ts';
import { DomainError } from '../../src/shared-kernel/index.ts';
import { makeDeps } from '../identity/fakes.ts';

const ORIGIN = 'https://mcp.example.com';

function setup(oidcConfigured: boolean) {
  const f = makeDeps();
  const app = new Hono<AppEnv>()
    .use(clientIp({ trustProxy: false, trustProxyHops: 1, remoteAddress: () => '192.0.2.7' }))
    .use(csrf(ORIGIN))
    .route(
      '/api',
      identityRoutes(f.deps, { cookieSecure: true, oidcConfigured, logError: () => undefined }),
    );
  app.onError((error, c) => {
    if (error instanceof DomainError) return domainErrorResponse(c, error);
    throw error;
  });
  return { f, app };
}

async function newCode(f: ReturnType<typeof makeDeps>, sub = 'ann'): Promise<string> {
  const p = await provisionUser(f.deps, { sub, name: sub, admin: false });
  if (!p.ok) throw p.error;
  const link = await issueLoginLink(f.deps, { sub });
  if (!link.ok) throw link.error;
  return link.value.code;
}

const post = (app: Hono<AppEnv>, body: unknown, headers: Record<string, string> = {}) =>
  app.request('/api/auth/link', {
    method: 'POST',
    headers: { 'content-type': 'application/json', origin: ORIGIN, ...headers },
    body: JSON.stringify(body),
  });

describe('GET /api/auth/methods', () => {
  it('says whether OIDC is configured, without a session', async () => {
    const off = await setup(false).app.request('/api/auth/methods');
    expect(off.status).toBe(200);
    expect(await off.json()).toEqual({ oidc: false });
    const on = await setup(true).app.request('/api/auth/methods');
    expect(await on.json()).toEqual({ oidc: true });
  });
});

describe('POST /api/auth/link', () => {
  it('signs in with a valid code: 204 and a session cookie that /me accepts', async () => {
    const { f, app } = setup(false);
    const code = await newCode(f);
    const res = await post(app, { code });
    expect(res.status).toBe(204);
    const cookie = res.headers.get('set-cookie') ?? '';
    expect(cookie).toMatch(/^mcp_session=/);
    expect(cookie).toContain('HttpOnly');
    expect(cookie).toContain('Secure');
    expect(cookie).toContain('SameSite=Strict');
    expect(cookie).toContain('Path=/');
    expect(cookie).not.toContain(code);
    const me = await app.request('/api/auth/me', {
      headers: { cookie: cookie.split(';')[0] ?? '' },
    });
    expect(me.status).toBe(200);
  });

  it('answers a used code with 401 invalid_link', async () => {
    const { f, app } = setup(false);
    const code = await newCode(f);
    await post(app, { code });
    const res = await post(app, { code });
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ error: { code: 'invalid_link' } });
  });

  it('answers 403 for a disabled person', async () => {
    const { f, app } = setup(false);
    const code = await newCode(f);
    for (const u of f.users.values()) u.disabled = true;
    const res = await post(app, { code });
    expect(res.status).toBe(403);
  });

  it('answers 429 with Retry-After when the limiter refuses', async () => {
    const { f, app } = setup(false);
    f.deps.linkLimiter.hit = () => ({ allowed: false, retryAfterSec: 42 });
    const res = await post(app, { code: 'nope' });
    expect(res.status).toBe(429);
    expect(res.headers.get('retry-after')).toBe('42');
  });

  it('answers 422 for a malformed body', async () => {
    const { app } = setup(false);
    expect((await post(app, {})).status).toBe(422);
    expect((await post(app, { code: '' })).status).toBe(422);
    expect((await post(app, { code: 'x'.repeat(101) })).status).toBe(422);
  });

  it('is refused by csrf without a matching Origin', async () => {
    const { f, app } = setup(false);
    const code = await newCode(f);
    const none = await app.request('/api/auth/link', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    expect(none.status).toBe(403);
    const other = await post(app, { code }, { origin: 'https://evil.example' });
    expect(other.status).toBe(403);
    // The code was not spent by the refused requests.
    expect((await post(app, { code })).status).toBe(204);
  });
});

describe('GET /api/auth/login without OIDC', () => {
  it('redirects to the UI with oidc_not_configured', async () => {
    const { app } = setup(false);
    const res = await app.request('/api/auth/login');
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('/?loginError=oidc_not_configured');
  });
});
