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
  type Page,
  type Response,
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

type Body =
  | { kind: 'text'; text: string }
  | { kind: 'unreadable'; why: string; excused: () => boolean };

interface Seen {
  url: string;
  method: string;
  body: Promise<Body>;
}

interface Watched {
  problems: string[];
  seen: Seen[];
  secrets: { value: string; fromIndex: number; label: string }[];
  excused: ExcusedRefusal[];
  /** How many times each frame has navigated (a frame can come back to the same URL). */
  navs: WeakMap<Frame, number>;
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

/**
 * The conditions under which a response whose body cannot be read is not a failure; every other
 * unreadable body is. This is the one place that lists them:
 *   - redirects (Playwright cannot read them), 204, 304, and answers to HEAD have no body;
 *   - binary assets (fonts, images, audio, video) are not read as text, and Chromium serves some of
 *     them (the preloaded font) without keeping a body to ask for; no secret is searched in them;
 *   - the request belonged to a frame that navigated away, was detached, or whose page was closed
 *     after the response: Chromium drops the bodies of the document that is left, and the
 *     response event fires before the body is complete, so a navigation that follows at once (the
 *     Connect button, a reload) can win the race even against a read started at the event. This
 *     is judged at the verdict, when the navigation has committed. It is the residual blind spot of
 *     reading bodies through Playwright, and the one place a leak could still hide.
 */
function legitimatelyUnreadable(
  r: Response,
  frame: Frame | null,
  navigated: () => boolean,
): boolean {
  const status = r.status();
  if ((status >= 300 && status < 400) || status === 204 || status === 304) return true;
  if (r.request().method() === 'HEAD') return true;
  if (/^(font|image|audio|video)\//.test(r.headers()['content-type'] ?? '')) return true;
  return frame !== null && (frame.isDetached() || frame.page().isClosed() || navigated());
}

async function readBody(r: Response, navs: WeakMap<Frame, number>): Promise<Body> {
  let frame: Frame | null = null;
  try {
    frame = r.request().frame();
  } catch {
    // a request without a frame (a worker's): never excused by navigation
  }
  const navsThen = frame === null ? 0 : (navs.get(frame) ?? 0);
  try {
    return { kind: 'text', text: await r.text() };
  } catch (e) {
    return {
      kind: 'unreadable',
      why: e instanceof Error ? (e.message.split('\n')[0] ?? '') : String(e),
      excused: () =>
        legitimatelyUnreadable(r, frame, () => frame !== null && (navs.get(frame) ?? 0) > navsThen),
    };
  }
}

async function watch(context: BrowserContext, w: Watched): Promise<void> {
  context.on('console', (m) => {
    if (m.type() !== 'error') return;
    const text = m.text();
    if (isExcused(w.excused, text, m.location().url, m.page())) return;
    w.problems.push(`console.error: ${text} (${m.location().url})`);
  });
  const countNavigations = (page: Page) =>
    page.on('framenavigated', (f) => w.navs.set(f, (w.navs.get(f) ?? 0) + 1));
  context.pages().forEach(countNavigations);
  context.on('page', countNavigations);
  context.on('weberror', (e) => w.problems.push(`page error: ${e.error().message}`));
  context.on('response', (response) => {
    // Read at once: a later navigation of the page drops the body.
    w.seen.push({
      url: response.url(),
      method: response.request().method(),
      body: readBody(response, w.navs),
    });
  });
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

/** The guard's verdict so far: every problem the test must not have. */
async function verdict(w: Watched): Promise<string[]> {
  const issues = [...w.problems];
  const bodies = await Promise.all(w.seen.map((s) => s.body));
  for (const [i, s] of w.seen.entries()) {
    const body = bodies[i];
    if (body === undefined) continue;
    if (body.kind === 'unreadable' && !body.excused()) {
      issues.push(
        `the body of the response to ${s.method} ${s.url} could not be read (${body.why}): the secret checks cannot see it`,
      );
    }
    const text = body.kind === 'text' ? body.text : '';
    const created = s.method === 'POST' && new URL(s.url).pathname === '/api/tokens';
    if (LINK_CODE.test(s.url) || LINK_CODE.test(text)) {
      issues.push(`a sign-in link code is in the response to ${s.method} ${s.url}`);
    }
    if (!created && API_TOKEN.test(text)) {
      issues.push(`an API token is in the response to ${s.method} ${s.url}`);
    }
    for (const sec of w.secrets) {
      if (i >= sec.fromIndex && text.includes(sec.value)) {
        issues.push(`${sec.label} is in the response to ${s.method} ${s.url}`);
      }
    }
  }
  return issues;
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
  /** Forgets everything seen so far: for the guard's own self-test, after it asserted a verdict. */
  reset: () => void;
}

export const test = base.extend<{ guard: Guard }>({
  guard: [
    async ({ context, browser }, use) => {
      const w: Watched = {
        problems: [],
        seen: [],
        secrets: [],
        excused: [...alwaysExcused],
        navs: new WeakMap(),
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
        problems: () => verdict(w),
        reset() {
          w.problems.length = 0;
          w.seen.length = 0;
          w.secrets.length = 0;
        },
      };
      await use(guard);

      const issues = await verdict(w);
      await Promise.all(extra.map((c) => c.close()));
      expect(issues, 'console errors, CSP violations and leaked secrets').toEqual([]);
    },
    { auto: true },
  ],
});

/** Signs `page` in with a link through the confirmation card, as a person does. Ends on /catalog. */
export async function signInWith(page: Page, link: string): Promise<void> {
  await page.goto(link);
  await page.getByRole('button', { name: strings.login.switchConfirm }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.catalog.title })).toBeVisible();
}
