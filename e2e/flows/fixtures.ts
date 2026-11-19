// Shared fixture of every flow. It makes two assertions for every test, in every browser context
// the test uses, without the test having to ask:
//   1. no console error, uncaught page error or CSP violation happened (the only exceptions are
//      the narrow, named ones below, or the refusals a flow declares with `excuseRefusal`);
//   2. a secret the UI showed once is gone after its dialog closed: not in the DOM, not in
//      localStorage or sessionStorage, and not in any response received afterwards. Besides the
//      secrets a test registers with `secretGone`, no response may ever carry a sign-in link code
//      (`ll_...`), and an API token (`sk_...`) may only be in the answer that created it.
import {
  test as base,
  expect,
  type BrowserContext,
  type Frame,
  type Request,
  type Page,
} from '@playwright/test';
import { strings } from '../../apps/web/src/strings.ts';

export { expect, strings };
export { control, getStacks, newPerson } from './control.ts';

/**
 * Chromium writes "Failed to load resource: the server responded with a status of <n> (...)" to
 * the console, as an error, for every fetch that is answered with a 4xx or 5xx, whatever the page
 * does with it. That is the browser's network log, not an error of the page, so a flow that makes
 * the server refuse a request on purpose names the exact status and path it excuses (and why).
 * Anything else, including the same message for another status or path, stays a failure.
 */
interface ExcusedRefusal {
  status: number;
  path: RegExp;
  why: string;
  /** Only messages of this page are excused; without it, those of any page of the test. */
  page?: Page | undefined;
}

const RESOURCE_ERROR = /^Failed to load resource: the server responded with a status of (\d+) /;

/**
 * Console messages excused for pages of the same-site stack (gateway.compat.test), whatever the
 * test: that stack is served over plain http under a name that is not localhost, which Chromium
 * treats as an untrustworthy origin and says so for the Cross-Origin-Opener-Policy header the
 * server sends. Real deployments are https. The text and the host are matched exactly.
 */
const untrustworthyOrigin =
  /^The Cross-Origin-Opener-Policy header has been ignored, because the URL's origin was untrustworthy\./;
const compatTestHost = /^[a-z]+\.compat\.test$/;

/**
 * Always excused: the SPA asks `/api/auth/me` on every load, and the documented answer of a
 * signed-out visitor is 401 (AuthProvider: "a 401 from /me means signed out, not an error").
 */
const alwaysExcused: ExcusedRefusal[] = [
  { status: 401, path: /^\/api\/auth\/me$/, why: 'the session probe of a signed-out visitor' },
];

/**
 * What reading a response's body came to:
 *   text        read;
 *   none        nothing to scan: a redirect, 204, 304, an answer to HEAD, or a binary asset;
 *   lost        the read failed AT THAT MOMENT while a navigation of the response's frame was under
 *               way or had committed since the response arrived (`reason` says which): the body
 *               went with the document that was left. Listed, never silent;
 *   unreadable  the read failed with no navigation going on: a failure.
 */
type Body =
  | { kind: 'text'; text: string }
  | { kind: 'none'; reason: string }
  | { kind: 'lost'; why: string; reason: string }
  | { kind: 'unreadable'; why: string };

interface Seen {
  url: string;
  method: string;
  status: number;
  /** Set once the read came to something; null while it is in flight. */
  body: Body | null;
  /** The read in flight, while it is. */
  pending: PendingRead | null;
  /** Resolves when `body` is set. */
  done: Promise<void>;
}

/**
 * A body read still in flight. A navigation of its frame (or the close of its page) that happens
 * while it is in flight marks it `affected`; the read itself may still succeed or fail. One that
 * never finishes is settled when the test ends: lost if affected, a failure if not.
 */
interface PendingRead {
  frame: Frame;
  finish: (b: Body) => void;
  affected: string | null;
  /** Where the read stands, for the failure message when it never finishes. */
  stage: string;
}

interface Watched {
  problems: string[];
  seen: Seen[];
  secrets: { value: string; fromIndex: number; label: string }[];
  excused: ExcusedRefusal[];
  /** How many times each frame has navigated (a frame can come back to the same URL). */
  navs: WeakMap<Frame, number>;
  /** Frames with a navigation request that has not committed or finished yet. */
  navigating: Set<Frame>;
  pending: Set<PendingRead>;
  /** The DevTools sessions being set up for pages opened later. */
  captures: Promise<void>[];
}

const LINK_CODE = /\bll_[A-Za-z0-9_-]{20,}/;
const API_TOKEN = /\bsk_[A-Za-z0-9_-]{20,}/;

declare global {
  interface Window {
    e2eReport?: (kind: string, detail: string) => void;
  }
}

function isExcused(
  excused: ExcusedRefusal[],
  text: string,
  url: string,
  page: Page | null,
): boolean {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }
  if (untrustworthyOrigin.test(text) && compatTestHost.test(parsed.hostname)) return true;
  const status = RESOURCE_ERROR.exec(text)?.[1];
  if (status === undefined) return false;
  return excused.some(
    (e) =>
      e.status === Number(status) &&
      e.path.test(parsed.pathname) &&
      (e.page === undefined || e.page === page),
  );
}

/** What the browser's network layer told about a response (via the DevTools protocol). */
interface Wire {
  url: string;
  method: string;
  status: number;
  mimeType: string;
  /** The response to a navigation of the frame (the document itself). */
  isDocument: boolean;
  frame: Frame;
  navsThen: number;
}

/**
 * Classifies a response whose body could not be read, at the moment the read failed. Only these
 * are not failures, and the verdict lists the ones that are `lost`:
 *   - none: redirects, 204, 304, answers to HEAD, binary assets (fonts, images, audio, video: not
 *     read as text) and the build's static files under /assets/ (the same bytes for everyone);
 *     no secret is searched in them;
 *   - lost: a navigation of the response's frame was already under way (its request had been sent
 *     and had not committed or finished), had committed since the response arrived, or the frame
 *     was detached or its page closed: Chromium drops the bodies of the document that is left.
 *     A navigation that happens LATER, after the read failed for another reason, excuses nothing.
 * Anything else, such as a request cut off by the server or aborted by the page itself, is
 * `unreadable`, a failure.
 */
function classifyFailure(wire: Wire, w: Watched, why: string): Body {
  const { status, frame } = wire;
  if ((status >= 300 && status < 400) || status === 204 || status === 304) {
    return { kind: 'none', reason: 'a redirect or an empty answer' };
  }
  if (wire.method === 'HEAD') return { kind: 'none', reason: 'an answer to HEAD' };
  if (/^(font|image|audio|video)\//.test(wire.mimeType)) {
    return { kind: 'none', reason: 'a binary asset' };
  }
  if (new URL(wire.url).pathname.startsWith('/assets/')) {
    // The build's scripts and styles, served from memory cache on a warm page, which Chromium then
    // cannot give a body for. They are the same bytes for everyone and carry no secret.
    return { kind: 'none', reason: 'a static build asset' };
  }
  if (frame.isDetached()) return { kind: 'lost', why, reason: 'its frame was detached' };
  if (frame.page().isClosed()) return { kind: 'lost', why, reason: 'its page was closed' };
  if ((w.navs.get(frame) ?? 0) > wire.navsThen) {
    return { kind: 'lost', why, reason: 'a navigation of its frame committed after the response' };
  }
  if (w.navigating.has(frame)) {
    return { kind: 'lost', why, reason: 'a navigation of its frame was under way' };
  }
  return { kind: 'unreadable', why };
}

const noop = (): void => undefined;

/**
 * Registers a response whose body is being read. The read can fail, or never finish (a fetch cut
 * off by a navigation neither completes nor fails). So a navigation of its frame (or the close of
 * its page) that happens while a read is in flight marks it affected; if it then fails, or never
 * finishes, it is `lost`. A read that never finishes and was not affected by a navigation is a
 * failure when the test ends. The body of a navigation's own response is not marked by the
 * navigation it caused.
 */
function trackBody(wire: Wire, w: Watched): { seen: Seen; finish: (b: Body) => void } {
  let finish: (b: Body) => void = noop;
  const settled = new Promise<Body>((resolve) => {
    finish = resolve;
  });
  const seen: Seen = {
    url: wire.url,
    method: wire.method,
    status: wire.status,
    body: null,
    pending: null,
    done: Promise.resolve(),
  };
  seen.done = settled.then((b) => {
    seen.body = b;
    seen.pending = null;
  });
  if (!wire.isDocument) {
    const pending: PendingRead = {
      frame: wire.frame,
      finish,
      affected: null,
      stage: 'waiting for the browser to report the request finished',
    };
    seen.pending = pending;
    w.pending.add(pending);
    void settled.then(() => w.pending.delete(pending));
  }
  w.seen.push(seen);
  return { seen, finish };
}

interface CdpResponseReceived {
  requestId: string;
  type?: string;
  response: { url: string; status: number; mimeType: string };
}

/**
 * Reads the bodies of everything `page` receives through the DevTools protocol: the body is asked
 * for the moment the browser reports the request finished, not when the headers arrive, which is
 * what Playwright's own `response.text()` does and what loses it to a navigation that follows at
 * once. The `Network` domain is enabled before the page does anything else.
 */
async function captureBodies(context: BrowserContext, page: Page, w: Watched): Promise<void> {
  const cdp = await context.newCDPSession(page);
  const methods = new Map<string, string>();
  const tracked = new Map<
    string,
    { finish: (b: Body) => void; wire: Wire; stage: (s: string) => void }
  >();
  cdp.on(
    'Network.requestWillBeSent',
    (e: { requestId: string; request: { method: string }; redirectResponse?: unknown }) => {
      if (e.redirectResponse !== undefined) {
        tracked.get(e.requestId)?.finish({ kind: 'none', reason: 'a redirect or an empty answer' });
        tracked.delete(e.requestId);
      }
      methods.set(e.requestId, e.request.method);
    },
  );
  cdp.on('Network.responseReceived', (e: CdpResponseReceived) => {
    const frame = page.mainFrame(); // the app has no frames of its own
    const wire: Wire = {
      url: e.response.url,
      method: methods.get(e.requestId) ?? 'GET',
      status: e.response.status,
      mimeType: e.response.mimeType,
      isDocument: e.type === 'Document',
      frame,
      navsThen: w.navs.get(frame) ?? 0,
    };
    const { seen, finish } = trackBody(wire, w);
    tracked.set(e.requestId, {
      finish,
      wire,
      stage: (st) => {
        if (seen.pending !== null) seen.pending.stage = st;
      },
    });
    const early = classifyFailure({ ...wire, frame }, w, '');
    if (early.kind === 'none') finish(early); // nothing to read: redirect, empty, binary
  });
  cdp.on('Network.loadingFinished', (e: { requestId: string }) => {
    const t = tracked.get(e.requestId);
    if (t === undefined) return;
    tracked.delete(e.requestId);
    t.stage('asked the browser for the body');
    cdp.send('Network.getResponseBody', { requestId: e.requestId }).then(
      (r: { body: string; base64Encoded: boolean }) =>
        t.finish(
          r.base64Encoded
            ? { kind: 'none', reason: 'a binary body' }
            : { kind: 'text', text: r.body },
        ),
      (err: unknown) => {
        const why = err instanceof Error ? (err.message.split('\n')[0] ?? '') : String(err);
        t.finish(classifyFailure(t.wire, w, why));
      },
    );
  });
  cdp.on('Network.loadingFailed', (e: { requestId: string; errorText: string }) => {
    const t = tracked.get(e.requestId);
    if (t === undefined) return;
    tracked.delete(e.requestId);
    t.finish(classifyFailure(t.wire, w, `the request failed after its response: ${e.errorText}`));
  });
  await cdp.send('Network.enable');
}

/** Marks the reads in flight of the frames `match` accepts as affected by a navigation. */
function markReads(w: Watched, match: (f: Frame) => boolean, reason: string): void {
  for (const p of w.pending) {
    if (match(p.frame)) p.affected ??= reason;
  }
}

function endNavigation(r: Request, w: Watched): void {
  if (!r.isNavigationRequest()) return;
  try {
    w.navigating.delete(r.frame());
  } catch {
    // no frame, nothing was tracked
  }
}

async function watch(context: BrowserContext, w: Watched): Promise<void> {
  context.on('console', (m) => {
    if (m.type() !== 'error') return;
    const text = m.text();
    if (isExcused(w.excused, text, m.location().url, m.page())) return;
    w.problems.push(`console.error: ${text} (${m.location().url})`);
  });
  const trackNavigations = (page: Page) => {
    page.on('framenavigated', (f) => {
      w.navs.set(f, (w.navs.get(f) ?? 0) + 1);
      w.navigating.delete(f);
      markReads(
        w,
        (x) => x === f,
        'a navigation of its frame committed while its body was being read',
      );
    });
    page.on('close', () =>
      markReads(w, (x) => x.page() === page, 'its page was closed while its body was being read'),
    );
    page.on('requestfinished', (r) => endNavigation(r, w));
    page.on('requestfailed', (r) => endNavigation(r, w));
  };
  await Promise.all(
    context.pages().map(async (page) => {
      trackNavigations(page);
      await captureBodies(context, page, w);
    }),
  );
  context.on('page', (page) => {
    trackNavigations(page);
    // A page that closes before its session is up has nothing left to read.
    w.captures.push(captureBodies(context, page, w).catch(() => undefined));
  });
  context.on('request', (r) => {
    if (!r.isNavigationRequest()) return;
    try {
      w.navigating.add(r.frame());
    } catch {
      // a navigation request always has a frame; nothing to track otherwise
    }
  });
  context.on('weberror', (e) => w.problems.push(`page error: ${e.error().message}`));
  await context.exposeFunction('e2eReport', (kind: string, detail: string) => {
    w.problems.push(`${kind}: ${detail}`);
  });
  await context.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      window.e2eReport?.(
        'CSP violation',
        `${e.effectiveDirective} blocked ${e.blockedURI} at ${e.sourceFile}`,
      );
    });
  });
}

interface Verdict {
  /** What the test must not have. */
  problems: string[];
  /** Responses whose bodies were lost to a navigation and so were not scanned (not failures). */
  notScanned: string[];
}

/**
 * The guard's verdict so far. Reads still in flight are not counted yet, except at the end of the
 * test (`final`), where they are failures: a body that never finished cannot be scanned.
 */
async function verdict(w: Watched, final: boolean): Promise<Verdict> {
  const problems = [...w.problems];
  const notScanned: string[] = [];
  const unfinished = 'its body had not finished';
  if (final) {
    await Promise.all(w.captures);
    // Reads that were only just issued when the test ended get a moment to answer; one that does
    // not is settled below. (This waits for a read in flight, it does not retry anything.)
    await Promise.race([
      Promise.all(w.seen.map((s) => s.done)),
      new Promise((resolve) => setTimeout(resolve, 5000)),
    ]);
    for (const p of w.pending) {
      p.finish(
        p.affected === null
          ? { kind: 'unreadable', why: `${unfinished} when the test ended; it was ${p.stage}` }
          : { kind: 'lost', why: `${unfinished} when the test ended`, reason: p.affected },
      );
    }
    await Promise.all(w.seen.map((s) => s.done));
  }
  for (const [i, s] of w.seen.entries()) {
    // A read that is still in flight and was affected by a navigation counts as lost already.
    const body: Body | null =
      s.body ??
      (s.pending?.affected ? { kind: 'lost', why: unfinished, reason: s.pending.affected } : null);
    if (body === null) continue;
    const what = `${s.method} ${s.url}`;
    if (body.kind === 'unreadable') {
      problems.push(
        `the body of the response to ${what} could not be read (${body.why}) and no navigation explains it: the secret checks cannot see it`,
      );
    }
    if (body.kind === 'lost') {
      notScanned.push(`${what} -> ${s.status}: ${body.reason} (${body.why})`);
      const shown = w.secrets.find((sec) => i >= sec.fromIndex);
      if (new URL(s.url).pathname.startsWith('/api/') && shown !== undefined) {
        problems.push(
          `the body of the API response to ${what} was lost to a navigation after a secret was shown (${shown.label}), so it could not be scanned for it: make the page consume that response before it navigates`,
        );
      }
    }
    const text = body.kind === 'text' ? body.text : '';
    const created = s.method === 'POST' && new URL(s.url).pathname === '/api/tokens';
    if (LINK_CODE.test(s.url) || LINK_CODE.test(text)) {
      problems.push(`a sign-in link code is in the response to ${what}`);
    }
    if (!created && API_TOKEN.test(text)) {
      problems.push(`an API token is in the response to ${what}`);
    }
    for (const sec of w.secrets) {
      if (i >= sec.fromIndex && text.includes(sec.value)) {
        problems.push(`${sec.label} is in the response to ${what}`);
      }
    }
  }
  return { problems, notScanned };
}

export interface Guard {
  /**
   * This flow makes the server refuse requests on purpose: the browser's console line about a
   * response with `status` for a path matching `path` is not a failure. `why` is the reason;
   * `page` limits it to the messages of that page.
   */
  excuseRefusal: (status: number, path: RegExp, why: string, page?: Page) => void;
  /** A browser context of its own (another person, another tab group), watched like the first. */
  newContext: () => Promise<BrowserContext>;
  /**
   * Call right after the dialog that showed `value` closed: checks the DOM and the storages of
   * `page` at once and, from now on, every response received. `label` names it in failures.
   */
  secretGone: (page: Page, value: string, label: string) => Promise<void>;
  /** What the guard would report now (the fixture's teardown asserts it is empty). */
  problems: () => Promise<string[]>;
  /** The responses whose bodies were lost to a navigation and so not scanned, one line each. */
  notScanned: () => Promise<string[]>;
  /** Forgets everything seen so far: for the guard's own self-test, after it asserted a verdict. */
  reset: () => void;
}

export const test = base.extend<{ guard: Guard }>({
  guard: [
    async ({ context, browser }, use, testInfo) => {
      const w: Watched = {
        problems: [],
        seen: [],
        secrets: [],
        excused: [...alwaysExcused],
        navs: new WeakMap(),
        navigating: new Set(),
        pending: new Set(),
        captures: [],
      };
      const extra: BrowserContext[] = [];
      await watch(context, w);
      const guard: Guard = {
        excuseRefusal(status, path, why, page) {
          w.excused.push({ status, path, why, page });
        },
        async newContext() {
          const c = await browser.newContext();
          await watch(c, w);
          extra.push(c);
          return c;
        },
        async secretGone(page, value, label) {
          expect(value.length, `${label}: the secret to look for is empty`).toBeGreaterThan(8);
          const dom = await page.evaluate(() => {
            const fields = [...document.querySelectorAll('input, textarea')].map((f) =>
              f instanceof HTMLInputElement || f instanceof HTMLTextAreaElement ? f.value : '',
            );
            let local = '';
            let session = '';
            try {
              local = JSON.stringify(Object.entries(window.localStorage));
              session = JSON.stringify(Object.entries(window.sessionStorage));
            } catch {
              // storage unavailable: nothing stored there
            }
            return { html: document.documentElement.outerHTML, fields, local, session };
          });
          expect(dom.html, `${label} is still in the DOM`).not.toContain(value);
          expect(dom.fields.join('\n'), `${label} is still in a form field`).not.toContain(value);
          expect(dom.local, `${label} is in localStorage`).not.toContain(value);
          expect(dom.session, `${label} is in sessionStorage`).not.toContain(value);
          expect(await page.content(), `${label} is in page.content()`).not.toContain(value);
          w.secrets.push({ value, fromIndex: w.seen.length, label });
        },
        problems: async () => (await verdict(w, false)).problems,
        notScanned: async () => (await verdict(w, false)).notScanned,
        reset() {
          w.problems.length = 0;
          w.seen.length = 0;
          w.secrets.length = 0;
          w.pending.clear();
        },
      };
      await use(guard);

      const { problems, notScanned } = await verdict(w, true);
      for (const line of notScanned) {
        // Not silent: what the guard could not scan is printed and attached to the test.
        console.log(`[guard] ${testInfo.titlePath.at(-1) ?? ''}: body not scanned: ${line}`);
        testInfo.annotations.push({ type: 'body-not-scanned', description: line });
      }
      await Promise.all(extra.map((c) => c.close()));
      expect(problems, 'console errors, CSP violations and leaked secrets').toEqual([]);
    },
    { auto: true },
  ],
});

/** Signs `page` in with a link through the confirmation card, as a person does. Ends on /catalog. */
export async function signInWith(page: Page, link: string): Promise<void> {
  await page.goto(link);
  const catalogAnswered = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/api/catalog' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: strings.login.switchConfirm }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.catalog.title })).toBeVisible();
  // The catalog the landing page asks for is received completely before the flow goes on and
  // navigates, so that the guard can read its body (a navigation drops an unfinished one).
  await (await catalogAnswered).finished();
}
