// The response guard of the flows: it sees EVERY response the browser receives in a test and sees to
// it that each one is scanned for secrets, or listed as not scanned with a reason, or fails the
// test. A response that is never recorded at all is impossible to hide: Playwright's own response
// events are cross-checked against the records at the verdict, page by page.
//
// State machine of one response (each transition has a self-test in 00-guard-selftest.spec.ts that
// asserts its outcome; the test names start with the transition's name):
//
//   recorded (the browser reported it through the DevTools protocol, or Playwright saw it)
//     |-- T1  body obtained (text, or base64 decoded) ........................... scanned
//     |-- T2  no body can exist: a redirect hop, 204, 304, an answer to HEAD ..... scanned (empty body)
//     |-- T3  content type is a real binary asset (font, audio, video, an image
//     |       other than SVG; SVG is text and goes to T1) ........................ body not scanned, listed
//     |-- T4  the read failed, the response came from the browser cache and the
//     |       application's own origin ........................................... body not scanned, listed
//     |-- T5  the read failed or never finished and a NEW DOCUMENT committed in its
//     |       frame after the response ........................................... body not scanned, listed;
//     |          a FAILURE if it is an /api/ body and a secret is registered, unless the flow declared
//     |          exactly that loss (`allowLostBody`: page + method + path, and only for this cause)
//     |-- T6  the read failed or never finished and its page was closed (after a REFUSED read that
//     |       nothing else explains, Playwright's close event is awaited within the grace, and only the
//     |       closed fact is re-read; the commit count stays frozen at the failure) .. same as T5, but no
//     |                                                                            declaration excuses it
//     |-- T7  the read failed with none of the above, judged on the facts AT the failure (a commit
//     |       after it changes nothing) ........................................... FAILURE (unreadable)
//     |-- T8  the read had not finished when the test ended (5 s grace) ......... T5/T6 when a new
//     |       document or the close had affected it, otherwise a FAILURE naming the stage
//     `-- T9  Playwright saw the response on a page and no record of that page exists .. FAILURE
//
// What does NOT excuse a failed read:
//   N2  a same-document (history API) route change, or a document's own commit (the baseline for its
//       own read): only a LATER cross-document commit of the main frame (`Page.frameNavigated`) counts;
//   N3  a navigation that has started but not committed: Chromium discards the old document's bodies
//       at the commit, not before, so a read made meanwhile succeeds (the self-test shows it).
//
// Scanning (S, H, U), at the verdict, so a secret registered late still applies to every response
// recorded before it: every registered secret, and the generic shapes of a sign-in link code
// (`ll_...`) and an API token (`sk_...`), are looked for in each record's URL (U), its response headers
// (H: those of `Network.responseReceived` and the raw ones of `Network.responseReceivedExtraInfo`,
// which include Set-Cookie) and its body (S). The only exemption is the body of the one response that
// legitimately carries a secret, declared when it is registered (page + method + path, at most one
// response); the generic `sk_` shape is allowed in the body of a `POST /api/tokens`.
//
// What cannot be made reliable in Chromium, stated plainly: a body that a navigation drops while the
// read is in flight cannot be recovered, so T5 and T6 are a blind spot by nature; they are listed,
// and an /api/ one after a registered secret fails, so a flow has to let the page consume it first.
import type { BrowserContext, CDPSession, Page } from '@playwright/test';

// The product's formats: a prefix plus the base64url of 32 random bytes (43 characters),
// internal/modules/identity (API tokens and sign-in link codes). The shape is a
// prefix, a left boundary and AT LEAST 41 base64url characters (44 in all, the product's 46 included),
// whatever follows: a longer token (another length a server may issue) is caught as well, and a random
// base64url value without the prefix (a session cookie) cannot match. Anything shorter than 44
// characters is not a credential of this shape; the registered secrets of the flows are matched exactly.
const LINK_CODE = /(?<![A-Za-z0-9_-])ll_[A-Za-z0-9_-]{41,}/;
const API_TOKEN = /(?<![A-Za-z0-9_-])sk_[A-Za-z0-9_-]{41,}/;

/** The bounded wait for captures, reads in flight and a page's close event. */
const GRACE_MS = 5000;

type NotScannedCause = 'binary' | 'cached' | 'new-document' | 'closed';

export type Outcome =
  | { kind: 'scanned'; text: string }
  | { kind: 'not-scanned'; reason: string; cause: NotScannedCause }
  | { kind: 'unreadable'; why: string };

function isLost(o: Outcome): boolean {
  return o.kind === 'not-scanned' && (o.cause === 'new-document' || o.cause === 'closed');
}

interface Rec {
  requestId: string;
  method: string;
  url: string;
  status: number;
  mimeType: string;
  /** The response headers the browser reported with the response (one `name: value` per line). */
  headers: string;
  isDocument: boolean;
  loaderId: string;
  cached: boolean;
  /** Cross-document commits of the frame when the response arrived (a document's own commit counts as the baseline). */
  navsThen: number;
  outcome: Outcome | null;
  /**
   * Set when a new document committed or the page closed while the body was still OPEN (before the
   * browser reported the request finished or failed). It decides only a read that never settles (T8,
   * and the interim verdict); a read that settled is judged by `settleFailure` alone.
   */
  affected: string | null;
  stage: string;
  done: Promise<void>;
  finish: (o: Outcome) => void;
  page: Page;
}

/** Raw response headers (`Network.responseReceivedExtraInfo`): the only place Set-Cookie shows. */
interface ExtraHeaders {
  requestId: string;
  status: number;
  headers: string;
  page: Page;
}

interface PageState {
  cdp: CDPSession;
  docNavs: number;
  committed: Set<string>;
  open: Map<string, Rec>;
  cached: Set<string>;
  methods: Map<string, string>;
  closed: boolean;
  /** Resolves when Playwright reports the page closed. */
  gone: Promise<void>;
}

/** The one response that legitimately carries a secret in its body (a token in its creating answer). */
export interface CarriedBy {
  page: Page;
  method: string;
  /** The exact path (`URL.pathname`). */
  path: string;
}

interface Secret {
  value: string;
  label: string;
  carriedBy?: CarriedBy | undefined;
}

export interface ExcusedRefusal {
  status: number;
  path: RegExp;
  why: string;
  /** Only messages of this page are excused; without it, those of any page of the test. */
  page?: Page | undefined;
}

interface AllowedLoss {
  page: Page;
  method: string;
  path: RegExp;
  why: string;
}

/** How a body is asked for: the DevTools call, unless a self-test replaces it. */
export type BodyRead = (
  cdp: CDPSession,
  requestId: string,
  page: Page,
  url: string,
) => Promise<{ body: string; base64Encoded: boolean }>;

const cdpBodyRead: BodyRead = (cdp, requestId) =>
  cdp.send('Network.getResponseBody', { requestId });

export interface Watched {
  problems: string[];
  records: Rec[];
  extraHeaders: ExtraHeaders[];
  /** What Playwright's own response events saw, for the cross-check. */
  seenByPlaywright: Seen[];
  secrets: Secret[];
  excused: ExcusedRefusal[];
  allowedLosses: AllowedLoss[];
  appOrigins: Set<string>;
  attaches: Map<Page, Promise<void>>;
  states: Map<Page, PageState>;
  pageIds: Map<Page, number>;
  readBody: BodyRead;
}

export function newWatched(excused: ExcusedRefusal[], appOrigins: string[]): Watched {
  return {
    problems: [],
    records: [],
    extraHeaders: [],
    seenByPlaywright: [],
    secrets: [],
    excused,
    allowedLosses: [],
    appOrigins: new Set(appOrigins),
    attaches: new Map(),
    states: new Map(),
    pageIds: new Map(),
    readBody: cdpBodyRead,
  };
}

const watchedOfPage = new WeakMap<Page, Watched>();

/** The guard state of a page (set when its capture is attached). */
export function watchedOf(page: Page): Watched | undefined {
  return watchedOfPage.get(page);
}

/** A short, stable name of a page in this test, for the cross-check and its messages. */
export function pageId(w: Watched, page: Page | null): string {
  if (page === null) return 'no page';
  let id = w.pageIds.get(page);
  if (id === undefined) {
    id = w.pageIds.size + 1;
    w.pageIds.set(page, id);
  }
  return `page ${id}`;
}

export function registerSecret(
  w: Watched,
  value: string,
  label: string,
  carriedBy?: CarriedBy,
): void {
  if (value.length <= 8) throw new Error(`${label}: the secret to look for is too short`);
  w.secrets.push({ value, label, carriedBy });
}

/** The facts the classification of a failed read depends on (separate so it can be tested alone). */
export interface FailureFacts {
  navsThen: number;
  navsNow: number;
  pageClosed: boolean;
  cached: boolean;
  appOrigin: boolean;
}

/** T4, T5, T6 and T7: what a read that failed (or never finished) comes to. */
export function classifyFailure(f: FailureFacts, why: string): Outcome {
  if (f.navsNow > f.navsThen) {
    return {
      kind: 'not-scanned',
      reason: `a new document committed in its frame after the response (${why})`,
      cause: 'new-document',
    };
  }
  if (f.pageClosed) {
    return { kind: 'not-scanned', reason: `its page was closed (${why})`, cause: 'closed' };
  }
  if (f.cached && f.appOrigin) {
    return { kind: 'not-scanned', reason: `cached asset (${why})`, cause: 'cached' };
  }
  return { kind: 'unreadable', why };
}

const BINARY_TYPE = /^(?:font|audio|video)\/|^image\/(?!svg\+xml)/;

function stripHash(url: string): string {
  const i = url.indexOf('#');
  return i < 0 ? url : url.slice(0, i);
}

function bodiless(status: number, method: string): boolean {
  return (status >= 300 && status < 400) || status === 204 || status === 304 || method === 'HEAD';
}

function headerLines(headers: Record<string, string> | undefined): string {
  return Object.entries(headers ?? {})
    .map(([name, value]) => `${name}: ${value}`)
    .join('\n');
}

interface CdpResponse {
  url: string;
  status: number;
  mimeType: string;
  headers?: Record<string, string>;
  fromDiskCache?: boolean;
  fromPrefetchCache?: boolean;
}
interface CdpRequestWillBeSent {
  requestId: string;
  loaderId?: string;
  type?: string;
  request: { method: string };
  redirectResponse?: CdpResponse;
}
interface CdpResponseReceived {
  requestId: string;
  loaderId?: string;
  type?: string;
  response: CdpResponse;
}
interface CdpResponseReceivedExtraInfo {
  requestId: string;
  statusCode: number;
  headers?: Record<string, string>;
  headersText?: string;
}

function makeRec(
  page: Page,
  st: PageState,
  e: Omit<Rec, 'done' | 'finish' | 'outcome' | 'affected' | 'stage' | 'navsThen' | 'page'>,
): Rec {
  let finish: (o: Outcome) => void = noop;
  const settled = new Promise<Outcome>((resolve) => {
    finish = resolve;
  });
  const rec: Rec = {
    ...e,
    page,
    navsThen: st.docNavs,
    outcome: null,
    affected: null,
    stage: 'waiting for the browser to report the request finished',
    done: Promise.resolve(),
    finish,
  };
  rec.done = settled.then((o) => {
    rec.outcome ??= o;
  });
  return rec;
}

const noop = (): void => undefined;

function failureFacts(w: Watched, st: PageState, rec: Rec): FailureFacts {
  return {
    navsThen: rec.navsThen,
    navsNow: st.docNavs,
    pageClosed: st.closed,
    cached: rec.cached,
    appOrigin: w.appOrigins.has(new URL(rec.url).origin),
  };
}

/**
 * Settles a read that failed. The facts are frozen when the failure arrives; only the page-closed
 * fact may be refreshed afterwards.
 *
 * The commit count is taken at the failure, without a further round trip, because a commit that
 * drops a body is reported before the failure: Chromium holds a `Network.getResponseBody` made
 * while a navigation is under way and refuses it only after the commit, and `Page.frameNavigated`
 * and that refusal travel on the same DevTools session in order (observed: frameNavigated, then the
 * refusal, for a 200 document and for an error-page commit). A `Network.loadingFailed` is an event
 * of the same session too. A later commit therefore never turns a failure into a loss.
 *
 * Playwright's close event, however, travels on another channel and can arrive after the DevTools
 * call was refused for the closing page. So when a `readBody` call was REFUSED and nothing else
 * decided the outcome (it would be T7), the close event is awaited within the grace and only
 * `pageClosed` is re-read. A `loadingFailed`, or an outcome already decided (a commit, a close, a
 * cached asset), is settled at once. The self-tests pin the order of `loadingFailed` and the commit.
 */
async function settleFailure(
  w: Watched,
  st: PageState,
  rec: Rec,
  why: string,
  refusedCall: boolean,
): Promise<void> {
  const facts = failureFacts(w, st, rec);
  const first = classifyFailure(facts, why);
  if (!refusedCall || first.kind !== 'unreadable') {
    rec.finish(first);
    return;
  }
  rec.stage = `the read was refused (${why}) and its page's close event was awaited`;
  await withDeadline(st.gone, GRACE_MS);
  rec.finish(classifyFailure({ ...facts, pageClosed: st.closed }, why));
}

/**
 * Attaches the capture to `page`: a DevTools session with the Network and Page domains enabled and
 * ACKNOWLEDGED. Idempotent per page. The guard calls it before it hands a page to a test.
 */
export function attachPage(context: BrowserContext, page: Page, w: Watched): Promise<void> {
  const existing = w.attaches.get(page);
  if (existing !== undefined) return existing;
  const attached = doAttach(context, page, w);
  w.attaches.set(page, attached);
  return attached;
}

/** `attachPage` within the grace, or an error that says which page could not be captured. */
export async function attachOrFail(
  context: BrowserContext,
  page: Page,
  w: Watched,
  what: string,
): Promise<void> {
  let failure = '';
  const attached = attachPage(context, page, w).then(
    () => true,
    (e: unknown) => {
      failure = e instanceof Error ? e.message : String(e);
      return false;
    },
  );
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<'late'>((resolve) => {
    timer = setTimeout(() => resolve('late'), GRACE_MS);
  });
  const result = await Promise.race([attached, late]);
  clearTimeout(timer);
  if (result === 'late') {
    throw new Error(`the guard could not attach its capture to ${what} within ${GRACE_MS} ms`);
  }
  if (!result) throw new Error(`the guard could not attach its capture to ${what}: ${failure}`);
}

async function doAttach(context: BrowserContext, page: Page, w: Watched): Promise<void> {
  watchedOfPage.set(page, w);
  pageId(w, page);
  const cdp = await context.newCDPSession(page);
  let markGone: () => void = noop;
  const st: PageState = {
    cdp,
    docNavs: 0,
    committed: new Set(),
    open: new Map(),
    cached: new Set(),
    methods: new Map(),
    closed: false,
    gone: new Promise<void>((resolve) => {
      markGone = resolve;
    }),
  };
  w.states.set(page, st);
  page.on('close', () => {
    st.closed = true;
    for (const rec of st.open.values()) rec.affected ??= 'its page was closed';
    markGone();
  });

  cdp.on('Network.requestWillBeSent', (e: CdpRequestWillBeSent) => {
    if (e.redirectResponse !== undefined) {
      // A redirect hop: no body can exist; its headers (Location) and URL are scanned.
      const prev = st.methods.get(e.requestId) ?? 'GET';
      const hop = makeRec(page, st, {
        requestId: e.requestId,
        method: prev,
        url: e.redirectResponse.url,
        status: e.redirectResponse.status,
        mimeType: e.redirectResponse.mimeType,
        headers: headerLines(e.redirectResponse.headers),
        isDocument: false,
        loaderId: e.loaderId ?? '',
        cached: false,
      });
      hop.finish({ kind: 'scanned', text: '' });
      w.records.push(hop);
    }
    st.methods.set(e.requestId, e.request.method);
  });
  cdp.on('Network.requestServedFromCache', (e: { requestId: string }) => {
    st.cached.add(e.requestId);
  });
  cdp.on('Network.responseReceivedExtraInfo', (e: CdpResponseReceivedExtraInfo) => {
    w.extraHeaders.push({
      requestId: e.requestId,
      status: e.statusCode,
      headers: e.headersText ?? headerLines(e.headers),
      page,
    });
  });
  cdp.on('Page.frameNavigated', (e: { frame: { parentId?: string; loaderId: string } }) => {
    if (e.frame.parentId !== undefined) return; // a new document of the MAIN frame only
    st.docNavs++;
    st.committed.add(e.frame.loaderId);
    for (const rec of st.open.values()) {
      if (rec.isDocument && rec.loaderId === e.frame.loaderId) {
        rec.navsThen = st.docNavs; // its own commit is the baseline for its own read
      } else {
        rec.affected ??= 'a new document committed in its frame while its body was being read';
      }
    }
  });
  cdp.on('Network.responseReceived', (e: CdpResponseReceived) => {
    const isDocument = e.type === 'Document';
    const rec = makeRec(page, st, {
      requestId: e.requestId,
      method: st.methods.get(e.requestId) ?? 'GET',
      url: e.response.url,
      status: e.response.status,
      mimeType: e.response.mimeType,
      headers: headerLines(e.response.headers),
      isDocument,
      loaderId: e.loaderId ?? e.requestId,
      cached:
        st.cached.has(e.requestId) ||
        e.response.fromDiskCache === true ||
        e.response.fromPrefetchCache === true,
    });
    if (isDocument && st.committed.has(rec.loaderId)) rec.navsThen = st.docNavs;
    w.records.push(rec);
    if (bodiless(rec.status, rec.method)) {
      rec.finish({ kind: 'scanned', text: '' });
    } else if (BINARY_TYPE.test(rec.mimeType)) {
      rec.finish({
        kind: 'not-scanned',
        reason: `binary asset (content type ${rec.mimeType})`,
        cause: 'binary',
      });
    } else {
      st.open.set(e.requestId, rec);
    }
  });
  cdp.on('Network.loadingFinished', (e: { requestId: string }) => {
    const rec = st.open.get(e.requestId);
    if (rec === undefined) return;
    st.open.delete(e.requestId);
    rec.stage = 'asked the browser for the body';
    w.readBody(cdp, e.requestId, page, rec.url).then(
      (r) => {
        // T1. A base64 body is a body Chromium does not treat as text: decoded and scanned all the same.
        const text = r.base64Encoded ? Buffer.from(r.body, 'base64').toString('latin1') : r.body;
        rec.finish({ kind: 'scanned', text });
      },
      (err: unknown) => {
        const why = err instanceof Error ? (err.message.split('\n')[0] ?? '') : String(err);
        void settleFailure(w, st, rec, why, true);
      },
    );
  });
  cdp.on('Network.loadingFailed', (e: { requestId: string; errorText: string }) => {
    const rec = st.open.get(e.requestId);
    if (rec === undefined) return;
    st.open.delete(e.requestId);
    void settleFailure(w, st, rec, `the request failed after its response: ${e.errorText}`, false);
  });
  await cdp.send('Network.enable');
  await cdp.send('Page.enable');
}

interface Seen {
  page: string;
  key: string;
  text: string;
}

/** Playwright's own view, for the cross-check: every context the guard watches reports here. */
export function watchPlaywrightResponses(context: BrowserContext, w: Watched): void {
  context.on('response', (r) => {
    let page: Page | null = null;
    try {
      page = r.frame().page();
    } catch {
      // a response without a frame (a service worker's): it can match no page's record
    }
    const method = r.request().method();
    const url = stripHash(r.url());
    w.seenByPlaywright.push({
      page: pageId(w, page),
      key: `${method} ${url}`,
      text: `${method} ${url} -> ${r.status()}`,
    });
  });
}

/**
 * T9: every response Playwright saw on a page needs a record of THAT page (a multiset match by
 * page + `METHOD URL`), so a missing record on one page cannot be masked by a spare one on another.
 */
export function crossCheck(recorded: { page: string; key: string }[], seen: Seen[]): string[] {
  const have = new Map<string, number>();
  for (const r of recorded) {
    const k = `${r.page} ${r.key}`;
    have.set(k, (have.get(k) ?? 0) + 1);
  }
  const problems: string[] = [];
  for (const s of seen) {
    const k = `${s.page} ${s.key}`;
    const n = have.get(k) ?? 0;
    if (n === 0) problems.push(`response never captured: ${s.text} (${s.page})`);
    else have.set(k, n - 1);
  }
  return problems;
}

export interface Verdict {
  problems: string[];
  /** Responses not scanned, with the reason: listed, never silent. */
  notScanned: string[];
}

async function withDeadline(work: Promise<unknown>, ms: number): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<boolean>((resolve) => {
    timer = setTimeout(() => resolve(false), ms);
  });
  const ok = await Promise.race([work.then(() => true), late]);
  clearTimeout(timer);
  return ok;
}

/** Where a secret or a shape was found, for the message. */
function scanPart(
  problems: string[],
  w: Watched,
  where: string,
  text: string,
  exempt: (s: Secret) => boolean,
  tokenShapeAllowed: boolean,
): void {
  if (text === '') return;
  if (LINK_CODE.test(text)) problems.push(`a sign-in link code is in ${where}`);
  if (!tokenShapeAllowed && API_TOKEN.test(text)) problems.push(`an API token is in ${where}`);
  for (const s of w.secrets) {
    if (text.includes(s.value) && !exempt(s)) problems.push(`${s.label} is in ${where}`);
  }
}

const never = (): boolean => false;

/**
 * The verdict so far. At the end of the test (`final`) it also waits, within `graceMs`, for the
 * captures and reads still in flight, settles the ones that do not finish (T8), and cross-checks
 * Playwright's responses against the records (T9).
 */
export async function verdict(w: Watched, final: boolean, graceMs = GRACE_MS): Promise<Verdict> {
  const problems = [...w.problems];
  if (final) {
    const attached = await withDeadline(Promise.allSettled(w.attaches.values()), graceMs);
    if (!attached) problems.push('the capture of a page was not attached within the grace period');
    await Promise.all(
      [...w.attaches].map(([page, p]) =>
        p.catch((e: unknown) => {
          if (!page.isClosed()) {
            problems.push(`the capture of a page could not be attached: ${String(e)}`);
          }
        }),
      ),
    );
    // Everything the browser emitted before this round trip has reached the sessions.
    await withDeadline(
      Promise.allSettled([...w.states.values()].map((st) => st.cdp.send('Page.getFrameTree'))),
      graceMs,
    );
    await withDeadline(Promise.all(w.records.map((r) => r.done)), graceMs);
    for (const rec of w.records) {
      if (rec.outcome !== null) continue;
      rec.finish(
        rec.affected === null
          ? {
              kind: 'unreadable',
              why: `its body had not finished when the test ended; it was ${rec.stage}`,
            }
          : {
              kind: 'not-scanned',
              reason: `${rec.affected} (its body had not finished when the test ended)`,
              cause: rec.affected.startsWith('its page was closed') ? 'closed' : 'new-document',
            },
      );
    }
    await withDeadline(Promise.all(w.records.map((r) => r.done)), graceMs);

    problems.push(
      ...crossCheck(
        w.records.map((r) => ({
          page: pageId(w, r.page),
          key: `${r.method} ${stripHash(r.url)}`,
        })),
        w.seenByPlaywright,
      ),
    );
  }

  // The one response each secret may legitimately be carried by (its body), at most once.
  const carrierOf = new Map<Secret, Rec>();
  for (const s of w.secrets) {
    const c = s.carriedBy;
    if (c === undefined) continue;
    const carrier = w.records.find(
      (r) =>
        r.page === c.page &&
        r.method === c.method &&
        new URL(r.url).pathname === c.path &&
        r.outcome?.kind === 'scanned' &&
        r.outcome.text.includes(s.value),
    );
    if (carrier !== undefined) carrierOf.set(s, carrier);
  }

  const notScanned: string[] = [];
  for (const rec of w.records) {
    // A read still in flight that a navigation or a close affected counts as lost already.
    const outcome: Outcome | null =
      rec.outcome ??
      (rec.affected === null
        ? null
        : {
            kind: 'not-scanned',
            reason: `${rec.affected} (still being read)`,
            cause: rec.affected.startsWith('its page was closed') ? 'closed' : 'new-document',
          });
    const what = `${rec.method} ${rec.url}`;
    const path = new URL(rec.url).pathname;
    // U and H do not depend on the body: scanned whatever happened to it.
    scanPart(problems, w, `the URL of ${what}`, rec.url, never, false);
    scanPart(problems, w, `the headers of the response to ${what}`, rec.headers, never, false);
    if (outcome === null) continue;
    if (outcome.kind === 'unreadable') {
      problems.push(`unreadable: ${what} -> ${rec.status} — ${outcome.why}`);
    }
    if (outcome.kind === 'not-scanned') {
      notScanned.push(`${what} -> ${rec.status}: ${outcome.reason}`);
      const secret = w.secrets[0];
      const allowed = w.allowedLosses.some(
        (a) =>
          outcome.cause === 'new-document' &&
          a.page === rec.page &&
          a.method === rec.method &&
          a.path.test(path),
      );
      if (isLost(outcome) && path.startsWith('/api/') && secret !== undefined && !allowed) {
        problems.push(
          `the body of the API response to ${what} was lost (${outcome.reason}) while a secret is registered (${secret.label}), so it could not be scanned for it: make the page consume that response before it navigates`,
        );
      }
    }
    if (outcome.kind === 'scanned') {
      const created = rec.method === 'POST' && path === '/api/tokens';
      scanPart(
        problems,
        w,
        `the response to ${what}`,
        outcome.text,
        (s) => carrierOf.get(s) === rec,
        created,
      );
    }
  }

  // Raw headers (Set-Cookie among them), named after the response they belong to.
  for (const x of w.extraHeaders) {
    const owner =
      w.records.find(
        (r) => r.page === x.page && r.requestId === x.requestId && r.status === x.status,
      ) ?? w.records.find((r) => r.page === x.page && r.requestId === x.requestId);
    const what =
      owner === undefined
        ? `an unrecorded response (request ${x.requestId}, ${pageId(w, x.page)})`
        : `${owner.method} ${owner.url}`;
    scanPart(problems, w, `the raw headers of the response to ${what}`, x.headers, never, false);
  }
  return { problems, notScanned };
}
