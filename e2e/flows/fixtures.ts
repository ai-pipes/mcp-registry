// Shared fixture of every flow. It makes these assertions for every test, in every browser context
// and page the test uses, without the test having to ask:
//   1. no console error, uncaught page error or CSP violation happened (the only exceptions are
//      the narrow, named ones below, or the refusals a flow declares with `excuseRefusal`);
//   2. every response the browser received is scanned, or listed as not scanned with a reason, or
//      fails the test (guard.ts has the state machine): no sign-in link code (`ll_...`) in any
//      response URL, header or body, an API token (`sk_...`) only in the body of the answer that
//      created it, and no registered secret (`guard.secret`) in any response of the test, also one
//      received before it was registered, except the one response declared to carry it;
//   3. a secret the UI showed once is gone after its dialog closed: not in the DOM, not in
//      localStorage or sessionStorage (`guard.secretGone`).
// The guard owns page creation: the page of the test, and every other page or context, exists only
// after its capture is attached and acknowledged (`guard.newPage`, `guard.newTab`). The checks are
// proved to fire by 00-guard-selftest.spec.ts.
import { test as base, expect, type BrowserContext, type Page } from '@playwright/test';
import { strings } from '../../apps/web/src/strings.ts';
import { getStacks } from './control.ts';
import {
  attachOrFail,
  attachPage,
  newWatched,
  pageId,
  registerSecret,
  verdict,
  watchedOf,
  watchPlaywrightResponses,
  type BodyRead,
  type CarriedBy,
  type ExcusedRefusal,
  type Verdict,
  type Watched,
} from './guard.ts';

export { expect, strings };
export { control, getStacks, newPerson } from './control.ts';

/**
 * Chromium writes "Failed to load resource: the server responded with a status of <n> (...)" to
 * the console, as an error, for every fetch that is answered with a 4xx or 5xx, whatever the page
 * does with it. That is the browser's network log, not an error of the page, so a flow that makes
 * the server refuse a request on purpose names the exact status and path it excuses (and why).
 * Anything else, including the same message for another status or path, stays a failure.
 */
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

declare global {
  interface Window {
    e2eReport?: (kind: string, detail: string) => void;
  }
}

/** Listeners and bindings of a context. Its pages are captured by `attachPage`. */
async function watchContext(context: BrowserContext, w: Watched): Promise<void> {
  context.on('console', (m) => {
    if (m.type() !== 'error') return;
    const text = m.text();
    if (isExcused(w.excused, text, m.location().url, m.page())) return;
    w.problems.push(`console.error: ${text} (${m.location().url})`);
  });
  context.on('weberror', (e) => w.problems.push(`page error: ${e.error().message}`));
  watchPlaywrightResponses(context, w);
  // A page the guard did not create (one the app opened itself): captured as soon as it appears; a
  // response it received before that is caught by the cross-check at the verdict.
  context.on('page', (page) => {
    void attachPage(context, page, w).catch(() => undefined); // reported at the verdict
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

export interface Guard {
  /**
   * This flow makes the server refuse requests on purpose: the browser's console line about a
   * response with `status` for a path matching `path` is not a failure. `why` is the reason;
   * `page` limits it to the messages of that page.
   */
  excuseRefusal: (status: number, path: RegExp, why: string, page?: Page) => void;
  /**
   * A response body that is lost by design to the new document it causes (the Connect button's
   * answer carries the provider's address and the page leaves at once): it is still listed, but the
   * `/api/` body of exactly this page, method and path does not fail the test for being lost while
   * a secret is registered. It excuses only a loss to a new document: not a closed page, not an
   * unreadable or uncaptured response.
   */
  allowLostBody: (page: Page, method: string, path: RegExp, why: string) => void;
  /** A page in a context of its own (another person), captured before it is returned. */
  newPage: () => Promise<Page>;
  /** Another tab in the context of `page`, captured before it is returned. */
  newTab: (page: Page) => Promise<Page>;
  /**
   * Registers a secret: every response of the test (also those received before this call) is
   * scanned for `value` in its URL, headers and body, and an `/api/` body lost in the test fails it.
   * `carriedBy` names the one response whose body legitimately carries it (a token's creating
   * answer): page + method + exact path, exempt at most once.
   */
  secret: (value: string, label: string, carriedBy?: CarriedBy) => void;
  /**
   * Call right after the dialog that showed `value` closed: checks the DOM and the storages of
   * `page` at once (and registers the secret if it is not yet). `label` names it in failures.
   */
  secretGone: (page: Page, value: string, label: string) => Promise<void>;
  /** What the guard would report now (the fixture's teardown asserts it is empty). */
  problems: () => Promise<string[]>;
  /** The responses not scanned, with the reason, one line each. */
  notScanned: () => Promise<string[]>;
  /** The end-of-test verdict (captures, reads in flight, cross-check) with a short grace period. */
  settle: (graceMs: number) => Promise<Verdict>;
  /** Forgets everything seen so far: for the guard's own self-test, after it asserted a verdict. */
  reset: () => void;
  /** Self-test hook: pretends Playwright saw a response on `page` that no record exists for. */
  injectUncaptured: (page: Page, method: string, url: string, status: number) => void;
  /** Self-test hook: replaces how bodies are asked for (to produce a failure at a chosen moment). */
  replaceBodyRead: (read: BodyRead) => void;
}

export const test = base.extend<{ guard: Guard }>({
  guard: [
    async ({ page, browser }, use, testInfo) => {
      const stacks = await getStacks();
      const w = newWatched(
        [
          {
            status: 401,
            path: /^\/api\/auth\/me$/,
            why: 'the session probe of a signed-out visitor',
          },
        ],
        Object.values(stacks).map((s) => new URL(s.baseURL).origin),
      );
      const extra: BrowserContext[] = [];
      const defaultRead = w.readBody;
      const context = page.context();
      // The page of the test exists already but has done nothing: listeners first, then its
      // capture, acknowledged, before the test gets it.
      await watchContext(context, w);
      await attachOrFail(context, page, w, 'the page of the test');
      const guard: Guard = {
        excuseRefusal(status, path, why, forPage) {
          w.excused.push({ status, path, why, page: forPage });
        },
        allowLostBody(forPage, method, path, why) {
          w.allowedLosses.push({ page: forPage, method, path, why });
        },
        async newPage() {
          const c = await browser.newContext();
          await watchContext(c, w);
          extra.push(c);
          const p = await c.newPage();
          await attachOrFail(c, p, w, 'a page in a new context');
          return p;
        },
        async newTab(of) {
          const c = of.context();
          const p = await c.newPage();
          await attachOrFail(c, p, w, 'a new tab');
          return p;
        },
        secret(value, label, carriedBy) {
          registerSecret(w, value, label, carriedBy);
        },
        async secretGone(forPage, value, label) {
          expect(value.length, `${label}: the secret to look for is empty`).toBeGreaterThan(8);
          const dom = await forPage.evaluate(() => {
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
          expect(await forPage.content(), `${label} is in page.content()`).not.toContain(value);
          if (!w.secrets.some((s) => s.value === value)) registerSecret(w, value, label);
        },
        problems: async () => (await verdict(w, false)).problems,
        notScanned: async () => (await verdict(w, false)).notScanned,
        settle: (graceMs) => verdict(w, true, graceMs),
        reset() {
          w.problems.length = 0;
          w.records.length = 0;
          w.extraHeaders.length = 0;
          w.seenByPlaywright.length = 0;
          w.secrets.length = 0;
          w.allowedLosses.length = 0;
          w.readBody = defaultRead;
        },
        injectUncaptured(forPage, method, url, status) {
          w.seenByPlaywright.push({
            page: pageId(w, forPage),
            key: `${method} ${url}`,
            text: `${method} ${url} -> ${status}`,
          });
        },
        replaceBodyRead(read) {
          w.readBody = read;
        },
      };
      await use(guard);

      const { problems, notScanned } = await verdict(w, true);
      const counts = new Map<string, number>();
      for (const line of notScanned) counts.set(line, (counts.get(line) ?? 0) + 1);
      for (const [line, n] of counts) {
        // Not silent: what the guard did not scan is printed and attached to the test.
        const text = n === 1 ? line : `${line} (x${n})`;
        console.log(`[guard] ${testInfo.titlePath.at(-1) ?? ''}: not scanned: ${text}`);
        testInfo.annotations.push({ type: 'not-scanned', description: text });
      }
      await Promise.all(extra.map((c) => c.close()));
      expect(
        problems,
        'console errors, CSP violations, leaked secrets and unscanned responses',
      ).toEqual([]);
    },
    { auto: true },
  ],
});

/**
 * The shell of a signed-in page has asked for the gateway's version and shown it. Waiting for it
 * lets that answer be consumed before the flow navigates to a new document (a navigation drops a
 * body that is still being read, and the guard fails an /api/ body lost while a secret is registered).
 */
export async function shellReady(page: Page): Promise<void> {
  await expect(page.getByRole('complementary').getByText(/^v\d+\.\d+\.\d+/)).toBeVisible();
}

/**
 * Signs `page` in with a link through the confirmation card, as a person does. Ends on /catalog.
 * The link's code is a secret from the moment it is used: it must not come back in any response.
 */
export async function signInWith(page: Page, link: string): Promise<void> {
  const w = watchedOf(page);
  const code = link.split('#link=')[1];
  if (w !== undefined && code !== undefined) registerSecret(w, code, 'the sign-in link code');
  await page.goto(link);
  const catalogAnswered = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/api/catalog' && r.request().method() === 'GET',
  );
  await page.getByRole('button', { name: strings.login.switchConfirm }).click();
  await expect(page.getByRole('heading', { level: 1, name: strings.catalog.title })).toBeVisible();
  // The catalog the landing page asks for is received completely before the flow goes on and
  // navigates, so that the guard can read its body (a navigation drops an unfinished one).
  await (await catalogAnswered).finished();
  await shellReady(page);
}
