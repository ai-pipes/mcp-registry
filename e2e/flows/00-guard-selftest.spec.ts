// The guard of fixtures.ts is what every other flow leans on for "no console error, no CSP
// violation, no secret in a later response". These tests prove that it fires: each makes the thing
// happen on purpose and asserts the guard's own verdict (`guard.problems()`), then clears it so the
// fixture's teardown does not fail the test for what was done deliberately.
import { readdirSync, readFileSync } from 'node:fs';
import { createServer, type Server, type ServerResponse } from 'node:http';
import type { Page } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { classifyFailure, crossCheck, type BodyRead } from './guard.ts';
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
  guard.secret(token, 'the probe token', { page, method: 'POST', path: '/api/tokens' });
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
  expect(problems.some((p) => p.includes('unreadable:'))).toBe(false); // nothing was blind
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

// A tiny server of its own for the tests of how a response comes to be scanned, listed or failed:
// the browser reaches it on 127.0.0.1, and a page it serves may fetch its own origin without the
// app's CSP.
const SECRET = 'a-secret-value-that-must-not-come-back';

// A value of the generic sign-in link code shape (`ll_` and 20 or more characters).
const LINK_SHAPED = 'll_abcdefghijklmnopqrstuvwxyz';

interface Tiny {
  origin: string;
  server: Server;
  held: ServerResponse[];
  /** Resolves when the browser asked for /hold-doc (a document whose answer is held). */
  docRequested: Promise<void>;
  /** Answers the held /hold-doc: a document (200), or no document at all (204). */
  releaseDoc: (status: 200 | 204) => void;
  /** Ends the held /api/held body with the secret in it. */
  finishHeldBody: () => void;
}

const nothing = (): void => undefined;

async function tinyServer(): Promise<Tiny> {
  const held: ServerResponse[] = [];
  let heldBody: ServerResponse | null = null;
  let heldDoc: ServerResponse | null = null;
  let markDocRequested: () => void = nothing;
  const docRequested = new Promise<void>((resolve) => {
    markDocRequested = resolve;
  });
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
    } else if (url === '/img.svg') {
      // SVG is text: scanned like any other text body.
      res.writeHead(200, { 'Content-Type': 'image/svg+xml' });
      res.end(`<svg xmlns="http://www.w3.org/2000/svg"><title>${SECRET}</title></svg>`);
    } else if (url === '/early' || url === '/api/tokens') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ token: SECRET }));
    } else if (url === '/redir-secret') {
      res.writeHead(302, { Location: `/ok?to=${SECRET}` }).end();
    } else if (url === '/cookie') {
      res.writeHead(200, {
        'Content-Type': 'text/plain',
        'Set-Cookie': [`probe=${SECRET}; Path=/`, `other=${LINK_SHAPED}; Path=/`],
      });
      res.end('a cookie was set');
    } else if (url === '/api/held') {
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.write('first chunk ');
      heldBody = res;
    } else if (url === '/hold-doc') {
      heldDoc = res;
      markDocRequested();
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
  return {
    origin: `http://127.0.0.1:${address.port}`,
    server,
    held,
    docRequested,
    releaseDoc: (status) => {
      if (status === 204) {
        heldDoc?.writeHead(204).end();
        return;
      }
      heldDoc?.writeHead(200, { 'Content-Type': 'text/html' });
      heldDoc?.end('<!doctype html><title>held</title><p>held document</p>');
    },
    finishHeldBody: () => heldBody?.end(`then the secret ${SECRET}`),
  };
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

const fetchOf = (page: Page, target: string, method = 'GET') =>
  page.evaluate(
    ([p, m]) =>
      fetch(p, { method: m })
        .then((r) => r.text())
        .catch(() => 'failed'),
    [target, method] as const,
  );

/** Starts a request whose body never ends (`/.../slow`) and waits for its response headers. */
async function startSlow(page: Page, origin: string, target: string, method = 'GET') {
  const answered = page.waitForResponse(
    (r) => r.url() === `${origin}${target}` && r.request().method() === method,
  );
  await page.evaluate(
    ([p, m]) => {
      void fetch(p, { method: m })
        .then((r) => r.text())
        .catch(() => 'cut');
    },
    [target, method] as const,
  );
  await answered;
}

/** A body lost to a new document: a never-ending request, then a navigation of its page. */
async function loseToNewDocument(page: Page, origin: string, target: string, method = 'GET') {
  await startSlow(page, origin, target, method);
  await page.goto(`${origin}/ok?after=${encodeURIComponent(method + target)}`);
}

const lostLines = async (guard: { problems: () => Promise<string[]> }) =>
  (await guard.problems()).filter((p) => p.includes('was lost'));

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
    pageClosed: false,
    cached: true,
    appOrigin: true,
  };
  expect(classifyFailure(facts, 'No resource with given identifier found')).toMatchObject({
    kind: 'not-scanned',
    cause: 'cached',
  });
  expect(classifyFailure({ ...facts, appOrigin: false }, 'x').kind).toBe('unreadable');
  expect(classifyFailure({ ...facts, cached: false }, 'x').kind).toBe('unreadable');
});

// --- T5 ---------------------------------------------------------------------------------------------
test('T5 a body cut by a new document in a test with no secret is listed, not a failure', async ({
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

test('T5 an /api/ body cut by a new document while a secret is registered fails', async ({
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
    // A declaration of a lost body does not reach an unreadable one.
    guard.allowLostBody(page, 'GET', /^\/broken$/, 'self-test');
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
test('T9 a response Playwright saw and no record exists for is a failure', async ({
  page,
  guard,
}) => {
  guard.injectUncaptured(page, 'GET', 'http://never.example.test/x', 200);
  const { problems } = await guard.settle(300);
  expect(problems.join('\n')).toMatch(
    /response never captured: GET http:\/\/never\.example\.test\/x -> 200 \(page \d+\)/,
  );
  guard.reset();
});

test('T9 a missing record on one page is not masked by a record of the same response on another', () => {
  const seen = { page: 'page 1', key: 'GET http://x.test/a', text: 'GET http://x.test/a -> 200' };
  expect(crossCheck([{ page: 'page 2', key: 'GET http://x.test/a' }], [seen])).toEqual([
    'response never captured: GET http://x.test/a -> 200 (page 1)',
  ]);
  // Negative control: the record of the same page matches.
  expect(crossCheck([{ page: 'page 1', key: 'GET http://x.test/a' }], [seen])).toEqual([]);
});

// --- T3 boundary: SVG is text ------------------------------------------------------------------------
test('T3 an SVG image is text: its body is scanned, not listed as a binary asset', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await fetchOf(page, '/img.svg');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the response to GET ${origin}/img.svg`);
    expect((await guard.notScanned()).join('\n')).not.toContain('/img.svg');
    guard.reset();
  });
});

// --- T5 declared loss (allowLostBody): exactly that page, method, path and cause ---------------------
test('T5 allowLostBody excuses only the declared page, method and path', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    const tab = await guard.newTab(page);
    await page.goto(`${origin}/ok`);
    await tab.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    guard.allowLostBody(page, 'POST', /^\/api\/declared\/slow$/, 'self-test');
    await loseToNewDocument(page, origin, '/api/declared/slow', 'POST'); // the declared one
    await loseToNewDocument(page, origin, '/api/other/slow', 'POST'); // another path
    await loseToNewDocument(page, origin, '/api/declared/slow', 'GET'); // another method
    await loseToNewDocument(tab, origin, '/api/declared/slow', 'POST'); // another page
    await expect.poll(async () => (await lostLines(guard)).length).toBe(3);
    const lost = (await lostLines(guard)).join('\n');
    expect(lost).toContain(
      `the body of the API response to POST ${origin}/api/other/slow was lost`,
    );
    expect(lost).toContain(
      `the body of the API response to GET ${origin}/api/declared/slow was lost`,
    );
    expect(lost.match(/to POST \S+\/api\/declared\/slow was lost/g)).toHaveLength(1); // the tab's
    // The declared one is still listed, never silent.
    expect((await guard.notScanned()).join('\n')).toContain(
      `POST ${origin}/api/declared/slow -> 200: a new document committed`,
    );
    guard.reset();
  });
});

test('T6 allowLostBody does not excuse a body lost to a closed page', async ({ page, guard }) => {
  await withTiny(async ({ origin }) => {
    const tab = await guard.newTab(page);
    await tab.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    guard.allowLostBody(tab, 'POST', /^\/api\/declared\/slow$/, 'self-test');
    await startSlow(tab, origin, '/api/declared/slow', 'POST');
    await tab.close();
    const { problems } = await guard.settle(1000);
    expect(problems.join('\n')).toContain(
      `the body of the API response to POST ${origin}/api/declared/slow was lost (its page was closed`,
    );
    guard.reset();
  });
});

// --- T6 a read that FAILS because its page closes, also when the close event comes after the refusal --
test('T6 a read refused while its page closes is lost to the close, also when the close event arrives after the refusal', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    const tab = await guard.newTab(page);
    await tab.goto(`${origin}/ok`);
    // The DevTools call is refused at once while the close is only starting: Playwright's close
    // event arrives after the refusal, which is the order the guard must not misjudge.
    const refuseAndClose: BodyRead = (_cdp, _id, p) => {
      if (p !== tab) return Promise.reject(new Error('the self-test reads only the tab'));
      void p.close();
      return Promise.reject(
        new Error('Target page, context or browser has been closed (self-test)'),
      );
    };
    guard.replaceBodyRead(refuseAndClose);
    await fetchOf(tab, '/ok?read').catch(() => 'closed');
    await expect
      .poll(async () => (await guard.notScanned()).join('\n'))
      .toContain(`GET ${origin}/ok?read -> 200: its page was closed`);
    expect((await guard.problems()).join('\n')).not.toContain('unreadable:');
    guard.reset();
  });
});

test('T7 a read refused while its page stays open is unreadable after the close event was awaited', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.replaceBodyRead(() => Promise.reject(new Error('refused by the self-test')));
    await fetchOf(page, '/ok?read');
    await expect
      .poll(async () => (await guard.problems()).join('\n'), { timeout: 10_000 })
      .toContain(`unreadable: GET ${origin}/ok?read -> 200 — refused by the self-test`);
    expect(await guard.notScanned()).toEqual([]);
    guard.reset();
  });
});

// --- N3 a navigation that has started but not committed excuses nothing --------------------------------
// Chromium does not answer a body read while a navigation of the page is under way: it holds the read
// and then fails it right after the commit (after `Page.frameNavigated`, which T5 counts), or answers
// it with the body when the navigation ends without a new document (204, a download). So a read never
// fails "during" a navigation, and only a commit excuses a lost body.

/** Fetches /api/held, starts a navigation to the held /hold-doc, then lets the body end. */
async function readDuringNavigation(page: Page, tiny: Tiny): Promise<void> {
  const answered = page.waitForResponse((r) => r.url() === `${tiny.origin}/api/held`);
  await page.evaluate(() => {
    void fetch('/api/held')
      .then((r) => r.text())
      .catch(() => 'cut');
  });
  await answered;
  await page.evaluate(() => {
    window.location.href = '/hold-doc';
  });
  await tiny.docRequested; // the navigation has started; the server holds its answer
  tiny.finishHeldBody(); // the body ends now, with the navigation under way
}

test('N3 a read made while a new document is under way is lost only by the commit (T5)', async ({
  page,
  guard,
}) => {
  await withTiny(async (tiny) => {
    await page.goto(`${tiny.origin}/ok`);
    await readDuringNavigation(page, tiny);
    expect(page.url()).toBe(`${tiny.origin}/ok`); // not committed yet
    tiny.releaseDoc(200);
    await page.waitForURL(`${tiny.origin}/hold-doc`);
    await expect
      .poll(async () => (await guard.notScanned()).join('\n'))
      .toContain(`GET ${tiny.origin}/api/held -> 200: a new document committed in its frame`);
    expect(await guard.problems()).toEqual([]);
    guard.reset();
  });
});

test('N3 a navigation that never commits excuses nothing: the held read is answered and scanned', async ({
  page,
  guard,
}) => {
  await withTiny(async (tiny) => {
    await page.goto(`${tiny.origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await readDuringNavigation(page, tiny);
    tiny.releaseDoc(204); // the navigation ends without a new document
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the response to GET ${tiny.origin}/api/held`);
    expect(page.url()).toBe(`${tiny.origin}/ok`); // still the same document
    expect((await guard.notScanned()).join('\n')).not.toContain('/api/held');
    guard.reset();
  });
});

// --- S: every registered secret applies to every record; one declared carrier, at most once ----------
test('S a secret registered after the response that carries it arrived is still caught', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    await fetchOf(page, '/early'); // arrives and is read before the secret is known
    expect(await guard.problems()).toEqual([]);
    guard.secret(SECRET, 'the probe secret');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the response to GET ${origin}/early`);
    guard.reset();
  });
});

test('S the declared creating response is exempt once; the same path again, another method or another page is not', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    const tab = await guard.newTab(page);
    await page.goto(`${origin}/ok`);
    await tab.goto(`${origin}/ok`);
    await fetchOf(page, '/api/tokens', 'POST'); // the creating answer
    guard.secret(SECRET, 'the probe secret', { page, method: 'POST', path: '/api/tokens' });
    expect(await guard.problems()).toEqual([]); // exempt
    await fetchOf(page, '/api/tokens', 'POST'); // the same path a second time
    await fetchOf(page, '/api/tokens', 'GET'); // another method
    await fetchOf(tab, '/api/tokens', 'POST'); // another page
    await expect
      .poll(async () =>
        (await guard.problems()).filter((p) => p.startsWith('the probe secret is in the response')),
      )
      .toHaveLength(3);
    const problems = (await guard.problems()).join('\n');
    expect(problems).toContain(`the probe secret is in the response to GET ${origin}/api/tokens`);
    expect(problems.match(/in the response to POST \S+\/api\/tokens/g)).toHaveLength(2);
    guard.reset();
  });
});

// --- H, U: headers and URLs are scanned -------------------------------------------------------------
test('H a secret in the Location header of a redirect is caught', async ({ page, guard }) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await fetchOf(page, '/redir-secret');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(
        `the probe secret is in the headers of the response to GET ${origin}/redir-secret`,
      );
    guard.reset();
  });
});

test('H a secret and a link-code shape in Set-Cookie headers are caught', async ({
  page,
  guard,
}) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await fetchOf(page, '/cookie');
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the raw headers of the response to GET ${origin}/cookie`);
    expect((await guard.problems()).join('\n')).toContain(
      `a sign-in link code is in the raw headers of the response to GET ${origin}/cookie`,
    );
    guard.reset();
  });
});

test('U a secret in the URL of a response is caught', async ({ page, guard }) => {
  await withTiny(async ({ origin }) => {
    await page.goto(`${origin}/ok`);
    guard.secret(SECRET, 'the probe secret');
    await fetchOf(page, `/ok?q=${SECRET}`);
    await expect
      .poll(async () => (await guard.problems()).join('\n'))
      .toContain(`the probe secret is in the URL of GET ${origin}/ok?q=${SECRET}`);
    guard.reset();
  });
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
