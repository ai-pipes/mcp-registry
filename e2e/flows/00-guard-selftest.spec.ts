// The guard of fixtures.ts is what every other flow leans on for "no console error, no CSP
// violation, no secret in a later response". These tests prove that it fires: each makes the thing
// happen on purpose and asserts the guard's own verdict (`guard.problems()`), then clears it so the
// fixture's teardown does not fail the test for what was done deliberately.
import { readdirSync, readFileSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, getStacks, newPerson, signInWith, strings, test } from './fixtures.ts';

test('the guard reports a secret that appears in a later response, also after a navigation', async ({
  page,
  guard,
}) => {
  const base = (await getStacks()).plain.baseURL;
  const person = await newPerson('plain', 'Guard Probe', false);
  await signInWith(page, person.link);
  await page.goto(`${base}/tokens`);
  await page.getByRole('button', { name: strings.tokens.create }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('textbox', { name: strings.tokens.name }).fill('probe');
  await dialog.getByRole('button', { name: strings.tokens.submit }).click();
  const token = await dialog.getByRole('textbox', { name: strings.tokens.token }).inputValue();
  await dialog.getByRole('button', { name: strings.tokens.done }).click();
  await expect(dialog).toBeHidden();
  await guard.secretGone(page, token, 'the probe token');
  expect(await guard.problems()).toEqual([]); // clean so far: the creating answer is exempt

  // A response served by the real network that carries the token: the control API's /echo, fetched
  // from a page of its own origin (the app's pages may not fetch other origins: strict CSP).
  const controlUrl = process.env['E2E_CONTROL_URL'] ?? '';
  const controlToken = process.env['E2E_CONTROL_TOKEN'] ?? '';
  const probe = await page.context().newPage();
  guard.excuseRefusal(
    401,
    /^\/$/,
    'the control API answers its root with 401: a same-origin page',
    probe,
  );
  await probe.goto(`${controlUrl}/`);
  await probe.evaluate(
    async ([bearer, text]) => {
      const r = await fetch('/echo', {
        method: 'POST',
        headers: { Authorization: `Bearer ${bearer}` },
        body: text,
      });
      await r.text();
    },
    [controlToken, token] as const,
  );
  await expect
    .poll(async () =>
      (await guard.problems()).some((p) =>
        p.includes('the probe token is in the response to POST'),
      ),
    )
    .toBe(true);
  await probe.close();

  // A later response that carries the token, served by the page's own route (what a server
  // leaking it would send).
  await page.route('**/api/tokens', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ leak: token }),
    }),
  );
  const answered = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/api/tokens' && r.request().method() === 'GET',
  );
  await page.reload();
  await answered;
  // A navigation right after: Chromium drops the bodies of the document that is left.
  await page.goto(`${base}/catalog`);

  const problems = await guard.problems();
  expect(problems.some((p) => p.includes('the probe token is in the response to GET'))).toBe(true);
  expect(problems.some((p) => p.includes('an API token is in the response to GET'))).toBe(true);
  expect(problems.some((p) => p.includes('could not be read'))).toBe(false); // nothing was blind
  guard.reset();
});

test('the guard reports a console error, a page error and a CSP violation', async ({
  page,
  guard,
}) => {
  const base = (await getStacks()).plain.baseURL;
  await page.goto(`${base}/`);
  await expect(page.getByRole('heading', { level: 1, name: strings.login.title })).toBeVisible();
  expect(await guard.problems()).toEqual([]);

  await page.evaluate(() => {
    console.error('self-test console error');
    setTimeout(() => {
      throw new Error('self-test page error');
    }, 0);
    const script = document.createElement('script');
    script.textContent = 'void 0'; // inline: the strict CSP refuses it
    document.head.appendChild(script);
  });

  await expect
    .poll(async () => (await guard.problems()).join('\n'), { timeout: 10_000 })
    .toContain('CSP violation: script-src');
  const problems = (await guard.problems()).join('\n');
  expect(problems).toContain('console.error: self-test console error');
  expect(problems).toContain('page error: self-test page error');
  guard.reset();
});

test('the guard excuses a refusal only for the page it was declared for', async ({
  page,
  guard,
}) => {
  const base = (await getStacks()).plain.baseURL;
  const other = await (await guard.newContext()).newPage();
  guard.excuseRefusal(404, /^\/api\/nothing-here$/, 'self-test', page);
  const refuse = (p: typeof page) =>
    p.goto(`${base}/`).then(() => p.evaluate(() => fetch('/api/nothing-here')));
  await refuse(page);
  expect(await guard.problems()).toEqual([]);
  await refuse(other);
  await expect.poll(async () => (await guard.problems()).join('\n')).toContain('status of 404');
  guard.reset();
});

test('no flow makes a browser context behind the guard', () => {
  const dir = path.dirname(fileURLToPath(import.meta.url));
  const self = path.basename(fileURLToPath(import.meta.url));
  const bypass =
    /\b(?:browser|chromium|firefox|webkit)\.(?:newContext|newPage|launch|launchPersistentContext)\s*\(/;
  const offenders = readdirSync(dir)
    .filter((f) => f.endsWith('.spec.ts') && f !== self)
    .filter((f) => bypass.test(readFileSync(path.join(dir, f), 'utf8')));
  expect(offenders, 'use guard.newContext() so the context is watched').toEqual([]);
});

// A tiny server of its own for the tests of unreadable bodies: the browser reaches it on
// 127.0.0.1, and a page it serves may fetch its own origin without the app's CSP.
async function tinyServer(): Promise<{ origin: string; server: Server }> {
  const server = createServer((req, res) => {
    const url = req.url ?? '';
    if (url.startsWith('/ok')) {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<!doctype html><title>ok</title><p>ok</p>');
    } else if (url === '/broken') {
      // Promises 1000 bytes, sends 7, drops the connection: the body can never be read.
      res.writeHead(200, { 'Content-Type': 'text/plain', 'Content-Length': '1000' });
      res.write('partial', () => res.socket?.destroy()); // after the headers and 7 bytes are out
    } else if (url.endsWith('/slow')) {
      // Headers and a first chunk, then silence until the client goes away.
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.write('first chunk');
    } else {
      res.writeHead(404).end();
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (address === null || typeof address === 'string')
    throw new Error('the tiny server has no port');
  return { origin: `http://127.0.0.1:${address.port}`, server };
}

test('a body that cannot be read with no navigation going on is reported', async ({
  page,
  guard,
}) => {
  const { origin, server } = await tinyServer();
  try {
    await page.goto(`${origin}/ok`);
    await page.evaluate(() =>
      fetch('/broken')
        .then((r) => r.text())
        .catch(() => 'failed'),
    );
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the body of the response to GET ${origin}/broken could not be read`);
    expect(await guard.notScanned()).toEqual([]); // not excused: nothing navigated
    guard.reset();
  } finally {
    server.closeAllConnections();
    server.close();
  }
});

test('a body lost to a navigation before any secret was shown is excused, and listed', async ({
  page,
  guard,
}) => {
  const { origin, server } = await tinyServer();
  try {
    await page.goto(`${origin}/ok`);
    const answered = page.waitForResponse((r) => r.url() === `${origin}/slow`);
    await page.evaluate(() => {
      void fetch('/slow')
        .then((r) => r.text())
        .catch(() => 'aborted');
    });
    await answered;
    await page.goto(`${origin}/ok?next`); // the navigation that takes the unfinished body with it
    await expect
      .poll(async () => (await guard.notScanned()).join('\n'))
      .toContain(`GET ${origin}/slow -> 200`);
    const problems = (await guard.problems()).join('\n');
    expect(problems).not.toContain('/slow'); // excused, not a failure
    guard.reset();
  } finally {
    server.closeAllConnections();
    server.close();
  }
});

test('an API body lost to a navigation after a secret was shown fails', async ({ page, guard }) => {
  const { origin, server } = await tinyServer();
  try {
    await page.goto(`${origin}/ok`);
    await guard.secretGone(page, 'a-secret-value-the-ui-showed-once', 'the probe secret');
    const answered = page.waitForResponse((r) => r.url() === `${origin}/api/slow`);
    await page.evaluate(() => {
      void fetch('/api/slow')
        .then((r) => r.text())
        .catch(() => 'aborted');
    });
    await answered;
    await page.goto(`${origin}/ok?next`);
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(
        `the body of the API response to GET ${origin}/api/slow was lost to a navigation after a secret was shown (the probe secret)`,
      );
    guard.reset();
  } finally {
    server.closeAllConnections();
    server.close();
  }
});
