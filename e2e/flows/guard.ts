// The response guard of the flows: it sees EVERY response the browser receives in a test and sees to
// it that each one is scanned for secrets, or listed as not scanned with a reason, or fails the
// test. A response that is never recorded at all is impossible to hide: Playwright's own response
// events are cross-checked against the records at the verdict.
//
// State machine of one response (the transitions are named, and each is covered by a self-test in
// 00-guard-selftest.spec.ts, named in brackets):
//
//   recorded (the browser reported it through the DevTools protocol, or Playwright saw it)
//     |-- T1  body obtained (text, or base64 decoded) ........................... scanned
//     |-- T2  no body can exist: a redirect hop, 204, 304, an answer to HEAD ..... scanned (empty)
//     |-- T3  content type is a real binary asset (font, image, audio, video) ... not scanned, listed
//     |-- T4  the read failed, the response came from the browser cache and the
//     |       application's own origin ........................................... not scanned, listed
//     |-- T5  the read failed or never finished and a NEW DOCUMENT committed (or was
//     |       under way) in its frame after the response ........................ not scanned, listed;
//     |                                      a FAILURE if it is an /api/ body after a secret was registered
//     |-- T6  the read failed or never finished and its page was closed ......... same as T5
//     |-- T7  the read failed with none of the above ............................ FAILURE (unreadable)
//     |-- T8  the read had not finished when the test ended (5 s grace) ......... T5/T6 when a new
//     |       document or the close had affected it, otherwise a FAILURE naming the stage
//     `-- T9  Playwright saw the response and no record exists .................. FAILURE (never captured)
//
// "A new document committed" means a cross-document commit of the main frame (the DevTools
// `Page.frameNavigated`); same-document (history API) route changes drop no body and count for
// nothing. A document response's own commit is the baseline for its own read: only a LATER commit,
// or another navigation under way, can excuse it.
//
// What cannot be made reliable in Chromium, stated plainly: a body that a navigation drops while the
// read is in flight cannot be recovered, so T5 and T6 are a blind spot by nature; they are listed,
// and an /api/ one after a registered secret fails, so a flow has to let the page consume it first.
import type { BrowserContext, CDPSession, Page } from '@playwright/test';

const LINK_CODE = /\bll_[A-Za-z0-9_-]{20,}/;
const API_TOKEN = /\bsk_[A-Za-z0-9_-]{20,}/;

export type Outcome =
  | { kind: 'scanned'; text: string }
  | { kind: 'not-scanned'; reason: string; lost: boolean }
  | { kind: 'unreadable'; why: string };

interface Rec {
  requestId: string;
  method: string;
  url: string;
  status: number;
  mimeType: string;
  isDocument: boolean;
  loaderId: string;
  cached: boolean;
  /** Cross-document commits of the frame when the response arrived (a document's own commit counts as the baseline). */
  navsThen: number;
  outcome: Outcome | null;
  /** Set when a new document committed or the page closed while the read was in flight. */
  affected: string | null;
  stage: string;
  done: Promise<void>;
  finish: (o: Outcome) => void;
  page: Page;
}

interface PageState {
  cdp: CDPSession;
  docNavs: number;
  navigating: Set<string>;
  committed: Set<string>;
  open: Map<string, Rec>;
  cached: Set<string>;
  methods: Map<string, string>;
  closed: boolean;
}

interface Secret {
  value: string;
  label: string;
  /** Records at the time it was registered: only later responses are scanned for it. */
  fromIndex: number;
}

export interface ExcusedRefusal {
  status: number;
  path: RegExp;
  why: string;
  /** Only messages of this page are excused; without it, those of any page of the test. */
  page?: Page | undefined;
}

interface AllowedLoss {
  path: RegExp;
  why: string;
  page?: Page | undefined;
}

export interface Watched {
  problems: string[];
  records: Rec[];
  /** What Playwright's own response events saw, for the cross-check. */
  seenByPlaywright: { key: string; text: string }[];
  secrets: Secret[];
  excused: ExcusedRefusal[];
  allowedLosses: AllowedLoss[];
  appOrigins: Set<string>;
  attaches: Map<Page, Promise<void>>;
  states: Map<Page, PageState>;
}

export function newWatched(excused: ExcusedRefusal[], appOrigins: string[]): Watched {
  return {
    problems: [],
    records: [],
    seenByPlaywright: [],
    secrets: [],
    excused,
    allowedLosses: [],
    appOrigins: new Set(appOrigins),
    attaches: new Map(),
    states: new Map(),
  };
}

const watchedOfPage = new WeakMap<Page, Watched>();

/** The guard state of a page (set when its capture is attached). */
export function watchedOf(page: Page): Watched | undefined {
  return watchedOfPage.get(page);
}

export function registerSecret(w: Watched, value: string, label: string): void {
  if (value.length <= 8) throw new Error(`${label}: the secret to look for is too short`);
  w.secrets.push({ value, label, fromIndex: w.records.length });
}

/** The facts the classification of a failed read depends on (separate so it can be tested alone). */
export interface FailureFacts {
  navsThen: number;
  navsNow: number;
  otherNavigationUnderWay: boolean;
  pageClosed: boolean;
  cached: boolean;
  appOrigin: boolean;
}

/** T4, T5, T6 and T7: what a read that failed (or never finished) comes to. */
export function classifyFailure(f: FailureFacts, why: string): Outcome {
  if (f.pageClosed) {
    return { kind: 'not-scanned', reason: `its page was closed (${why})`, lost: true };
  }
  if (f.navsNow > f.navsThen) {
    return {
      kind: 'not-scanned',
      reason: `a new document committed in its frame after the response (${why})`,
      lost: true,
    };
  }
  if (f.otherNavigationUnderWay) {
    return {
      kind: 'not-scanned',
      reason: `a navigation to a new document was under way (${why})`,
      lost: true,
    };
  }
  if (f.cached && f.appOrigin) {
    return { kind: 'not-scanned', reason: `cached asset (${why})`, lost: false };
  }
  return { kind: 'unreadable', why };
}

const BINARY_TYPE = /^(font|image|audio|video)\//;

function stripHash(url: string): string {
  const i = url.indexOf('#');
  return i < 0 ? url : url.slice(0, i);
}

function bodiless(status: number, method: string): boolean {
  return (status >= 300 && status < 400) || status === 204 || status === 304 || method === 'HEAD';
}

interface CdpRequestWillBeSent {
  requestId: string;
  loaderId?: string;
  type?: string;
  request: { method: string };
  redirectResponse?: { url: string; status: number; mimeType: string };
}
interface CdpResponseReceived {
  requestId: string;
  loaderId?: string;
  type?: string;
  response: {
    url: string;
    status: number;
    mimeType: string;
    fromDiskCache?: boolean;
    fromPrefetchCache?: boolean;
  };
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
    otherNavigationUnderWay: [...st.navigating].some((l) => l !== rec.loaderId),
    pageClosed: st.closed,
    cached: rec.cached,
    appOrigin: w.appOrigins.has(new URL(rec.url).origin),
  };
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

async function doAttach(context: BrowserContext, page: Page, w: Watched): Promise<void> {
  watchedOfPage.set(page, w);
  const cdp = await context.newCDPSession(page);
  const st: PageState = {
    cdp,
    docNavs: 0,
    navigating: new Set(),
    committed: new Set(),
    open: new Map(),
    cached: new Set(),
    methods: new Map(),
    closed: false,
  };
  w.states.set(page, st);
  page.on('close', () => {
    st.closed = true;
    for (const rec of st.open.values()) rec.affected ??= 'its page was closed';
  });

  cdp.on('Network.requestWillBeSent', (e: CdpRequestWillBeSent) => {
    if (e.redirectResponse !== undefined) {
      // A redirect hop: no body can exist, the hop is recorded as scanned (empty).
      const prev = st.methods.get(e.requestId) ?? 'GET';
      const hop = makeRec(page, st, {
        requestId: e.requestId,
        method: prev,
        url: e.redirectResponse.url,
        status: e.redirectResponse.status,
        mimeType: e.redirectResponse.mimeType,
        isDocument: false,
        loaderId: e.loaderId ?? '',
        cached: false,
      });
      hop.finish({ kind: 'scanned', text: '' });
      w.records.push(hop);
    }
    st.methods.set(e.requestId, e.request.method);
    if (e.type === 'Document') st.navigating.add(e.loaderId ?? e.requestId);
  });
  cdp.on('Network.requestServedFromCache', (e: { requestId: string }) => {
    st.cached.add(e.requestId);
  });
  cdp.on('Page.frameNavigated', (e: { frame: { parentId?: string; loaderId: string } }) => {
    if (e.frame.parentId !== undefined) return; // a new document of the MAIN frame only
    st.docNavs++;
    st.navigating.delete(e.frame.loaderId);
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
        lost: false,
      });
    } else {
      st.open.set(e.requestId, rec);
    }
  });
  cdp.on('Network.loadingFinished', (e: { requestId: string }) => {
    st.navigating.delete(e.requestId);
    const rec = st.open.get(e.requestId);
    if (rec === undefined) return;
    st.open.delete(e.requestId);
    rec.stage = 'asked the browser for the body';
    cdp.send('Network.getResponseBody', { requestId: e.requestId }).then(
      (r: { body: string; base64Encoded: boolean }) => {
        // T1. A base64 body is a body Chromium does not treat as text: decoded and scanned all the same.
        const text = r.base64Encoded ? Buffer.from(r.body, 'base64').toString('latin1') : r.body;
        rec.finish({ kind: 'scanned', text });
      },
      (err: unknown) => {
        const why = err instanceof Error ? (err.message.split('\n')[0] ?? '') : String(err);
        rec.finish(classifyFailure(failureFacts(w, st, rec), why));
      },
    );
  });
  cdp.on('Network.loadingFailed', (e: { requestId: string; errorText: string }) => {
    st.navigating.delete(e.requestId);
    const rec = st.open.get(e.requestId);
    if (rec === undefined) return;
    st.open.delete(e.requestId);
    rec.finish(
      classifyFailure(
        failureFacts(w, st, rec),
        `the request failed after its response: ${e.errorText}`,
      ),
    );
  });
  await cdp.send('Network.enable');
  await cdp.send('Page.enable');
}

/** Playwright's own view, for the cross-check: every context the guard watches reports here. */
export function watchPlaywrightResponses(context: BrowserContext, w: Watched): void {
  context.on('response', (r) => {
    const method = r.request().method();
    const url = stripHash(r.url());
    w.seenByPlaywright.push({
      key: `${method} ${url}`,
      text: `${method} ${url} -> ${r.status()}`,
    });
  });
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

/**
 * The verdict so far. At the end of the test (`final`) it also waits, within `graceMs`, for the
 * captures and reads still in flight, settles the ones that do not finish (T8), and cross-checks
 * Playwright's responses against the records (T9).
 */
export async function verdict(w: Watched, final: boolean, graceMs = 5000): Promise<Verdict> {
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
              lost: true,
            },
      );
    }
    await withDeadline(Promise.all(w.records.map((r) => r.done)), graceMs);

    // T9: every response Playwright saw must have a record.
    const have = new Map<string, number>();
    for (const rec of w.records) {
      const key = `${rec.method} ${stripHash(rec.url)}`;
      have.set(key, (have.get(key) ?? 0) + 1);
    }
    for (const seen of w.seenByPlaywright) {
      const n = have.get(seen.key) ?? 0;
      if (n === 0) problems.push(`response never captured: ${seen.text}`);
      else have.set(seen.key, n - 1);
    }
  }

  const notScanned: string[] = [];
  for (const [i, rec] of w.records.entries()) {
    // A read still in flight that a navigation or a close affected counts as lost already.
    const outcome: Outcome | null =
      rec.outcome ??
      (rec.affected === null
        ? null
        : { kind: 'not-scanned', reason: `${rec.affected} (still being read)`, lost: true });
    if (outcome === null) continue;
    const what = `${rec.method} ${rec.url}`;
    if (outcome.kind === 'unreadable') {
      problems.push(`unreadable: ${what} -> ${rec.status} — ${outcome.why}`);
    }
    if (outcome.kind === 'not-scanned') {
      notScanned.push(`${what} -> ${rec.status}: ${outcome.reason}`);
      const secret = w.secrets.find((s) => i >= s.fromIndex);
      const path = new URL(rec.url).pathname;
      const allowed = w.allowedLosses.some(
        (a) => a.path.test(path) && (a.page === undefined || a.page === rec.page),
      );
      if (outcome.lost && path.startsWith('/api/') && secret !== undefined && !allowed) {
        problems.push(
          `the body of the API response to ${what} was lost (${outcome.reason}) after a secret was registered (${secret.label}), so it could not be scanned for it: make the page consume that response before it navigates`,
        );
      }
    }
    const text = outcome.kind === 'scanned' ? outcome.text : '';
    const created = rec.method === 'POST' && new URL(rec.url).pathname === '/api/tokens';
    if (LINK_CODE.test(rec.url) || LINK_CODE.test(text)) {
      problems.push(`a sign-in link code is in the response to ${what}`);
    }
    if (!created && API_TOKEN.test(text)) {
      problems.push(`an API token is in the response to ${what}`);
    }
    for (const s of w.secrets) {
      if (i >= s.fromIndex && text.includes(s.value)) {
        problems.push(`${s.label} is in the response to ${what}`);
      }
    }
  }
  return { problems, notScanned };
}
