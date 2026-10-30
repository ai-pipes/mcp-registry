// The served UI under the served, strict CSP (see CLAUDE.md, Web UI): no violations in a real browser, and
// no <style> element at all (a dependency that starts inserting one fails here). Toasts are
// covered by Toaster.test.tsx.
import { expect, test } from '@playwright/test';

type Violation = { directive: string; blockedURI: string; sourceFile: string };

declare global {
  interface Window {
    cspViolations: Violation[];
  }
}

test('the UI loads without CSP violations', async ({ page }) => {
  const cspMessages: string[] = [];
  page.on('console', (m) => {
    if (/Content Security Policy/i.test(m.text())) cspMessages.push(m.text());
  });
  await page.addInitScript(() => {
    window.cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) =>
      window.cspViolations.push({
        directive: e.effectiveDirective,
        blockedURI: e.blockedURI,
        sourceFile: e.sourceFile,
      }),
    );
  });

  const res = await page.goto('/');
  expect(res?.headers()['content-security-policy']).toBe(
    "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
      "frame-ancestors 'none'",
  );
  await expect(page.locator('#root')).not.toBeEmpty();
  expect(await page.evaluate(() => document.querySelectorAll('style').length)).toBe(0);
  expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  expect(cspMessages).toEqual([]);
});

// Every page of the signed-in UI under the same CSP. The API is stubbed in the browser
// (`page.route`), so this needs no login; the HTML, scripts and CSS are the served ones.
const server = {
  slug: 'gitea',
  name: 'Gitea',
  description: 'Git hosting',
  url: 'https://gitea.example.com/mcp',
  auth: {
    kind: 'oauth_user',
    header: 'Authorization',
    template: 'Bearer {secret}',
    oauth: {
      authorizeUrl: 'https://gitea.example.com/login/oauth/authorize',
      tokenUrl: 'https://gitea.example.com/login/oauth/access_token',
      clientId: 'cid',
      scopes: ['read'],
      extraAuthorizeParams: { access_type: 'offline' },
    },
  },
  secretSet: true,
  access: { kind: 'all' },
  timeoutMs: 120000,
  dailyLimit: null,
  status: 'active',
  lastSyncAt: '2026-10-30T10:00:00Z',
  lastSyncError: 'OAuth provider unavailable',
  toolCount: 1,
};
const tool = {
  ref: 'gitea.create_issue',
  name: 'create_issue',
  title: null,
  description: 'Create an issue',
  annotations: { destructiveHint: true },
  inputSchema: {},
  outputSchema: null,
  hidden: false,
};
const stubs: Record<string, unknown> = {
  '/api/auth/me': { userId: 'u1', name: 'Anna', role: 'admin', groups: ['mcp-admins'] },
  '/api/catalog': [{ slug: 'gitea', name: 'Gitea', description: 'Git', tools: [tool] }],
  '/api/connections': [
    { server: 'gitea', name: 'Gitea', authKind: 'oauth_user', status: 'missing' },
    { server: 'grafana', name: 'Grafana', authKind: 'pat_user', status: 'ready' },
  ],
  '/api/tokens': [
    {
      id: 't1',
      name: 'laptop',
      prefix: 'sk_abcd',
      createdAt: '2026-10-30T10:00:00Z',
      lastUsedAt: null,
      revokedAt: null,
    },
  ],
  '/api/servers': [server],
  '/api/servers/gitea': server,
  '/api/servers/gitea/tools': [tool],
  '/api/users': [
    {
      id: 'u1',
      name: 'Anna',
      email: null,
      role: 'admin',
      groups: [],
      disabled: false,
      createdAt: '2026-10-01T10:00:00Z',
      lastLoginAt: null,
    },
  ],
  '/api/calls': [],
  '/api/audit': [],
};

const pages: [path: string, heading: string][] = [
  ['/catalog', 'Catalog'],
  ['/tokens', 'Tokens'],
  ['/connections', 'Connections'],
  ['/connections?connected=gitea', 'Connections'],
  ['/admin/servers', 'Servers'],
  ['/admin/servers/new', 'New server'],
  ['/admin/servers/gitea', 'Gitea'],
  ['/admin/users', 'Users'],
  ['/admin/calls', 'Call log'],
  ['/admin/audit', 'Audit'],
];

for (const [path, heading] of pages) {
  test(`${path} renders under the strict CSP`, async ({ page }) => {
    await page.addInitScript(() => {
      window.cspViolations = [];
      document.addEventListener('securitypolicyviolation', (e) =>
        window.cspViolations.push({
          directive: e.effectiveDirective,
          blockedURI: e.blockedURI,
          sourceFile: e.sourceFile,
        }),
      );
    });
    await page.route('**/api/**', async (route) => {
      const body = stubs[new URL(route.request().url()).pathname];
      await (body === undefined
        ? route.fulfill({ status: 404, json: { error: { code: 'not_found', message: 'x' } } })
        : route.fulfill({ json: body }));
    });
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1, name: heading })).toBeVisible();
    // Signed in as admin: the admin menu is part of the shell.
    await expect(page.getByRole('link', { name: 'Users' })).toBeVisible();
    expect(await page.evaluate(() => document.querySelectorAll('style').length)).toBe(0);
    expect(await page.evaluate(() => window.cspViolations)).toEqual([]);
  });
}
