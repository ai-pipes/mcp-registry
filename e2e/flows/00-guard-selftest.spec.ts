// The guard of fixtures.ts is what every other flow leans on for "no console error, no CSP
// violation, no secret in a later response". These tests prove that it fires: each makes the thing
// happen on purpose and asserts the guard's own verdict (`guard.problems()`), then clears it so the
// fixture's teardown does not fail the test for what was done deliberately.
import { readdirSync, readFileSync } from 'node:fs';
import { createServer, type Server, type ServerResponse } from 'node:http';
import type { Page } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { classifyFailure } from './guard.ts';
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
  guard.secret(token, 'the probe token');
  // The dialog ignores Done while the token list is still being refreshed behind it (a UI quirk,
  // see the report): wait for the new row, which proves the refresh is done, then close.
  await expect(page.getByRole('row', { name: /probe/, includeHidden: true })).toBeVisible();
  await dialog.getByRole('button', { name: strings.tokens.done }).click();
  await expect(dialog).toBeHidden();
  await guard.secretGone(page, token, 'the probe token');
  expect(await guard.problems()).toEqual([]); // clean so far: the creating answer is exempt

  // A response served by the real network that carries the token: the control API's /echo, fetched
  // from a page of its own origin (the app's pages may not fetch other origins: strict CSP).
  const controlUrl = process.env['E2E_CONTROL_URL'] ?? '';
  const controlToken = process.env['E2E_CONTROL_TOKEN'] ?? '';
  const probe = await guard.newTab(page);
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
  const other = await guard.newPage();
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
  // A page or context made by anything but the guard's own methods (a bare call counts too).
  // A page or context made by anything but the guard's own methods (a bare call counts too).
  const bypass =
    /(?<!guard)\.(?:newContext|newPage|launch|launchPersistentContext)\s*\(|(?<![.\w])(?:newContext|newPage)\s*\(/;
  const offenders = readdirSync(dir)
    .filter((f) => f.endsWith('.spec.ts') && f !== self)
    .filter((f) => bypass.test(readFileSync(path.join(dir, f), 'utf8')));
  expect(offenders, 'use guard.newContext() so the context is watched').toEqual([]);
});

// A tiny server of its own for the tests of how a response comes to be scanned, listed or failed:
// the browser reaches it on 127.0.0.1, and a page it serves may fetch its own origin without the
// app's CSP.
const SECRET = 'a-secret-value-that-must-not-come-back';

interface Tiny {
  origin: string;
  server: Server;
  held: ServerResponse[];
}

async function tinyServer(): Promise<Tiny> {
  const held: ServerResponse[] = [];
  const server = createServer((req, res) => {
    const url = req.url ?? '';
    if (url.startsWith('/ok')) {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<!doctype html><title>ok</title><p>ok</p>');
    } else if (url === '/first') {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end(`<!doctype html><title>first</title><p>${SECRET}</p><script src="/sub.js"></script>`);
    } else if (url === '/sub.js') {
      res.writeHead(200, { 'Content-Type': 'text/javascript' });
      res.end(`// ${SECRET}\n`);
    } else if (url === '/odd') {
      // A content type Chromium does not treat as text: its body comes back base64 encoded.
      res.writeHead(200, { 'Content-Type': 'application/x-odd' });
      res.end(`odd ${SECRET}`);
    } else if (url === '/img.png') {
      res.writeHead(200, { 'Content-Type': 'image/png' });
      res.end(Buffer.from([0x89, 0x50, 0x4e, 0x47]));
    } else if (url === '/redir') {
      res.writeHead(302, { Location: '/ok' }).end();
    } else if (url === '/broken') {
      // Promises 1000 bytes, sends 7, drops the connection: the body can never be read.
      res.writeHead(200, { 'Content-Type': 'text/plain', 'Content-Length': '1000' });
      res.write('partial', () => res.socket?.destroy());
    } else if (url.endsWith('/slow') || url === '/hold') {
      // Headers and a first chunk, then silence until the client goes away or /release.
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.write('first chunk');
      held.push(res);
    } else if (url === '/release') {
      for (const h of held.splice(0)) h.socket?.destroy();
      res.writeHead(200).end('released');
    } else {
      res.writeHead(404).end();
    }
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (address === null || typeof address === 'string')
    throw new Error('the tiny server has no port');
  return { origin: `http://127.0.0.1:${address.port}`, server, held };
}

async function withTiny(run: (t: Tiny) => Promise<void>): Promise<void> {
  const tiny = await tinyServer();
  try {
    await run(tiny);
  } finally {
    tiny.server.closeAllConnections();
    tiny.server.close();
  }
}

const fetchOf = (page: Page, target: string) =>
  page.evaluate(
    (p) =>
      fetch(p)
        .then((r) => r.text())
        .catch(() => 'failed'),
    target,
  );

// --- T1, N1: the FIRST document and a subresource of a fresh page are scanned ----------------------
test('T1 first load: a fresh page whose first document and subresource hold a secret is caught', async ({
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    const fresh = await guard.newPage(); // captured before it is returned, so its first request is seen
    guard.secret(SECRET, 'the probe secret');
    await fresh.goto(`${origin}/first`);
    await expect(fresh.getByText(SECRET)).toBeVisible();
    const problems = (await guard.settle(1000)).problems.join('\n');
    expect(problems).toContain(`the probe secret is in the response to GET ${origin}/first`);
    expect(problems).toContain(`the probe secret is in the response to GET ${origin}/sub.js`);
    expect(problems).not.toContain('response never captured');
    guard.reset();
  });
});

test('T1 first load: the page of the test is captured before its first request too', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    guard.secret(SECRET, 'the probe secret');
    await page.goto(`${origin}/first`); // the very first thing this page does
    const problems = (await guard.settle(1000)).problems.join('\n');
    expect(problems).toContain(`the probe secret is in the response to GET ${origin}/first`);
    expect(problems).toContain(`the probe secret is in the response to GET ${origin}/sub.js`);
    expect(problems).not.toContain('response never captured');
    guard.reset();
  });
});

// --- T1 (base64), N3 ---------------------------------------------------------------------------------
test('T1 base64: a body of a content type Chromium does not treat as text is decoded and scanned', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await fetchOf(page, '/odd');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the response to GET ${origin}/odd`);
    guard.reset();
  });
});

// --- T2 -------------------------------------------------------------------------------------------
test('T2 a redirect hop has no body: scanned empty, not listed, not a problem', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/redir`);
    const verdict = await guard.settle(1000);
    expect(verdict.problems).toEqual([]);
    expect(verdict.notScanned).toEqual([]);
    guard.reset();
  });
});

// --- T3 -------------------------------------------------------------------------------------------
test('T3 a real binary asset is listed as not scanned, with its content type', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    await fetchOf(page, '/img.png');
    const verdict = await guard.settle(1000);
    expect(verdict.notScanned.join('\n')).toContain(
      `GET ${origin}/img.png -> 200: binary asset (content type image/png)`,
    );
    expect(verdict.problems).toEqual([]);
    guard.reset();
  });
});

// --- T4 (the classification alone: a genuine cache-served read failure cannot be produced) -------
test('T4 a failed read of a cached asset on the app origin is listed, anywhere else it is a failure', () => {
  const facts = {
    navsThen: 1,
    navsNow: 1,
    otherNavigationUnderWay: false,
    pageClosed: false,
    cached: true,
    appOrigin: true,
  };
  expect(classifyFailure(facts, 'No resource with given identifier found')).toMatchObject({
    kind: 'not-scanned',
    lost: false,
  });
  expect(classifyFailure({ ...facts, appOrigin: false }, 'x').kind).toBe('unreadable');
  expect(classifyFailure({ ...facts, cached: false }, 'x').kind).toBe('unreadable');
});

// --- T5 ---------------------------------------------------------------------------------------------
test('T5 a body cut by a new document before any secret was registered is listed, not a failure', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    const answered = page.waitForResponse((r) => r.url() === `${origin}/slow`);
    await page.evaluate(() => {
      void fetch('/slow')
        .then((r) => r.text())
        .catch(() => 'aborted');
    });
    await answered;
    await page.goto(`${origin}/ok?next`);
    await expect
      .poll(async () => (await guard.notScanned()).join('\n'))
      .toContain(`GET ${origin}/slow -> 200: a new document committed`);
    expect((await guard.problems()).join('\n')).not.toContain('/slow');
    guard.reset();
  });
});

test('T5 an /api/ body cut by a new document after a secret was registered fails', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
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
      .toContain(`the body of the API response to GET ${origin}/api/slow was lost`);
    guard.reset();
  });
});

test('N2 a same-document route change excuses nothing: the failed read is a failure', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    const answered = page.waitForResponse((r) => r.url() === `${origin}/hold`);
    await page.evaluate(() => {
      void fetch('/hold')
        .then((r) => r.text())
        .catch(() => 'cut');
    });
    await answered;
    await page.evaluate(() => history.pushState({}, '', '/routed')); // what the SPA does on every route change
    await fetchOf(page, '/release'); // the server drops the held connection: the read fails
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`unreadable: GET ${origin}/hold -> 200`);
    expect(await guard.notScanned()).toEqual([]);
    guard.reset();
  });
});

// --- T6 ---------------------------------------------------------------------------------------------
test('T6 a body still being read when its page is closed is listed as lost to the close', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    const tab = await guard.newTab(page);
    await tab.goto(`${origin}/ok`);
    const answered = tab.waitForResponse((r) => r.url() === `${origin}/slow`);
    await tab.evaluate(() => {
      void fetch('/slow').then((r) => r.text());
    });
    await answered;
    await tab.close();
    await expect
      .poll(async () => (await guard.notScanned()).join('\n'))
      .toContain(`GET ${origin}/slow -> 200: its page was closed`);
    guard.reset();
  });
});

// --- T7 ---------------------------------------------------------------------------------------------
test('T7 a body that cannot be read with no new document going on is a failure', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    await fetchOf(page, '/broken');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`unreadable: GET ${origin}/broken -> 200`);
    expect(await guard.notScanned()).toEqual([]);
    guard.reset();
  });
});

test('N2 a document whose own read fails is not excused by its own commit', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/broken`, { waitUntil: 'commit' }).catch(() => undefined); // a truncated document
    await expect
      .poll(async () => (await guard.problems()).join('\n'), { timeout: 10_000 })
      .toContain(`unreadable: GET ${origin}/broken -> 200`);
    guard.reset();
  });
});

// --- T8 ---------------------------------------------------------------------------------------------
test('T8 a read that has not finished when the test ends is a failure naming the stage', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    const answered = page.waitForResponse((r) => r.url() === `${origin}/slow`);
    await page.evaluate(() => {
      void fetch('/slow').then((r) => r.text());
    });
    await answered;
    const { problems } = await guard.settle(300);
    expect(problems.join('\n')).toContain(
      `unreadable: GET ${origin}/slow -> 200 — its body had not finished when the test ended; it was waiting for the browser to report the request finished`,
    );
    guard.reset();
  });
});

// --- T9 (an injected missing record: a real one cannot be produced on demand) -----------------------
test('T9 a response Playwright saw and no record exists for is a failure', async ({ guard }) => {
  guard.injectUncaptured('GET', 'http://never.example.test/x', 200);
  const { problems } = await guard.settle(300);
  expect(problems).toContain('response never captured: GET http://never.example.test/x -> 200');
  guard.reset();
});

test('no flow makes a page or a context behind the guard', () => {
  const dir = path.dirname(fileURLToPath(import.meta.url));
  const self = path.basename(fileURLToPath(import.meta.url));
  const bypass =
    /(?<!guard)\.(?:newContext|newPage|launch|launchPersistentContext)\s*\(|(?<![.\w])(?:newContext|newPage)\s*\(/;
  const offenders = readdirSync(dir)
    .filter((f) => f.endsWith('.spec.ts') && f !== self)
    .filter((f) => bypass.test(readFileSync(path.join(dir, f), 'utf8')));
  expect(offenders, 'use guard.newPage() or guard.newTab() so the page is captured').toEqual([]);
});
