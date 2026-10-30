import { beforeEach, describe, expect, it } from 'vite-plus/test';
import type { AuthMode, ServerCallTarget, ToolView } from '../../catalog/index.ts';
import type { CredentialFailure } from '../../connections/index.ts';
import {
  err,
  ok,
  type Result,
  type ToolCallResult,
  UpstreamError,
  type UpstreamTarget,
} from '../../../shared-kernel/index.ts';
import { QuotaDay } from '../domain/quota-day.ts';
import { callTool } from './call-tool.ts';
import type {
  ArgsValidator,
  CallLog,
  CallLogEntry,
  GatewayDeps,
  PersonalCredentials,
  Principal,
  Quota,
  ToolCatalog,
} from './ports.ts';

const PUBLIC_URL = 'https://mcp.example.com';
const ALICE: Principal = { userId: 'alice', role: 'member', groups: ['dev'], tokenId: 'tok-a' };
const BOB: Principal = { userId: 'bob', role: 'member', groups: ['dev'], tokenId: 'tok-b' };

const BEARER = { header: 'Authorization', template: 'Bearer {secret}' };
const OAUTH: AuthMode = {
  kind: 'oauth_user',
  ...BEARER,
  oauth: { authorizeUrl: 'https://o/a', tokenUrl: 'https://o/t', clientId: 'c', scopes: [] },
};

type FakeServer = {
  target: ServerCallTarget;
  status: 'active' | 'disabled';
  groups: string[] | 'all';
  secret: string | null;
};
type FakeTool = { view: ToolView; hidden: boolean; removed: boolean };

/** The catalog's rules in miniature: active server, access by group, visible tool. */
class FakeCatalog implements ToolCatalog {
  readonly servers = new Map<string, FakeServer>();
  readonly tools = new Map<string, FakeTool>();

  addServer(slug: string, auth: AuthMode, extra: Partial<FakeServer> = {}, dailyLimit?: number) {
    const target: ServerCallTarget = {
      slug,
      name: slug === 'gitea' ? 'Gitea' : `Srv ${slug}`,
      url: `https://${slug}.example.com/mcp`,
      auth,
      timeoutMs: 30_000,
      dailyLimit: dailyLimit ?? null,
    };
    this.servers.set(slug, { target, status: 'active', groups: 'all', secret: null, ...extra });
    this.addTool(slug, 'create_issue');
  }

  addTool(server: string, name: string) {
    const view: ToolView = {
      server,
      name,
      ref: `${server}.${name}`,
      title: null,
      description: '',
      inputSchema: { type: 'object', properties: { title: { type: 'string' } } },
      outputSchema: null,
      annotations: {},
      hidden: false,
    };
    this.tools.set(view.ref, { view, hidden: false, removed: false });
  }

  toolFor(principal: Principal, ref: string): Promise<ToolView | null> {
    const tool = this.tools.get(ref);
    const server = tool ? this.servers.get(tool.view.server) : undefined;
    if (!tool || !server || tool.hidden || tool.removed || server.status !== 'active') {
      return Promise.resolve(null);
    }
    const allowed =
      principal.role === 'admin' ||
      server.groups === 'all' ||
      server.groups.some((g) => principal.groups.includes(g));
    return Promise.resolve(allowed ? tool.view : null);
  }

  serverFor(slug: string): Promise<ServerCallTarget | null> {
    const s = this.servers.get(slug);
    return Promise.resolve(s?.status === 'active' ? s.target : null);
  }

  serverSecret(slug: string): Promise<string | null> {
    return Promise.resolve(this.servers.get(slug)?.secret ?? null);
  }
}

/** Per-person tokens; `refreshTo` is what the next forced refresh hands out (or its failure). */
class FakeCredentials implements PersonalCredentials {
  readonly tokens = new Map<string, Result<string, CredentialFailure>>();
  readonly refreshTo = new Map<string, Result<string, CredentialFailure>>();
  readonly resolved: { userId: string; server: string; forceRefresh: boolean }[] = [];
  readonly expired: { userId: string; server: string }[] = [];

  set(userId: string, server: string, token: string) {
    this.tokens.set(`${userId}/${server}`, ok(token));
  }

  resolve(q: { userId: string; server: string; forceRefresh?: boolean }) {
    const key = `${q.userId}/${q.server}`;
    this.resolved.push({ userId: q.userId, server: q.server, forceRefresh: !!q.forceRefresh });
    if (q.forceRefresh) {
      const next = this.refreshTo.get(key) ?? err<CredentialFailure>('expired');
      this.tokens.set(key, next);
      return Promise.resolve(next);
    }
    return Promise.resolve(this.tokens.get(key) ?? err<CredentialFailure>('needs_connection'));
  }

  markExpired(q: { userId: string; server: string }) {
    this.expired.push(q);
    this.tokens.set(`${q.userId}/${q.server}`, err('expired'));
    return Promise.resolve();
  }
}

/** Upstream answers from a script: a result, or an UpstreamError to throw. */
class FakeUpstream {
  readonly calls: { target: UpstreamTarget; name: string; args: Record<string, unknown> }[] = [];
  readonly script: (ToolCallResult | UpstreamError)[] = [];
  fallback: ToolCallResult = { content: [{ type: 'text', text: 'done' }] };

  listTools(): Promise<never> {
    return Promise.reject(new Error('unused'));
  }

  callTool(target: UpstreamTarget, name: string, args: Record<string, unknown>) {
    this.calls.push({ target, name, args });
    const next = this.script.shift() ?? this.fallback;
    return next instanceof UpstreamError ? Promise.reject(next) : Promise.resolve(next);
  }
}

class FakeQuota implements Quota {
  readonly counts = new Map<string, number>();
  deleteDaysBefore() {
    return Promise.resolve(0);
  }
  tryConsume(i: { userId: string; server: string; day: string; limit: number }) {
    const key = `${i.userId}/${i.server}/${i.day}`;
    const n = this.counts.get(key) ?? 0;
    if (n >= i.limit)
      return Promise.resolve({ ok: false as const, resetAt: QuotaDay.resetAt(i.day) });
    this.counts.set(key, n + 1);
    return Promise.resolve({ ok: true as const });
  }
}

class FakeCallLog implements CallLog {
  readonly entries: CallLogEntry[] = [];
  record(e: CallLogEntry) {
    this.entries.push(e);
    return Promise.resolve();
  }
  list() {
    return Promise.resolve([]);
  }
  deleteOlderThan() {
    return Promise.resolve(0);
  }
}

/** `title` must be a string when present; anything else passes. */
const validator: ArgsValidator = {
  validate(_ref, _schema, args) {
    const title =
      typeof args === 'object' && args !== null && 'title' in args ? args.title : undefined;
    return title === undefined || typeof title === 'string'
      ? { ok: true }
      : { ok: false, path: '/title', message: 'must be string' };
  },
};

let catalog: FakeCatalog;
let credentials: FakeCredentials;
let upstream: FakeUpstream;
let quota: FakeQuota;
let log: FakeCallLog;
let clock: { current: Date; now(): Date };
let deps: GatewayDeps;
let errors: { fields: Record<string, unknown>; message: string }[];

beforeEach(() => {
  catalog = new FakeCatalog();
  credentials = new FakeCredentials();
  upstream = new FakeUpstream();
  quota = new FakeQuota();
  log = new FakeCallLog();
  clock = { current: new Date('2026-10-30T10:00:00Z'), now: () => clock.current };
  errors = [];
  deps = {
    logger: {
      error: (fields, message) => {
        errors.push({ fields, message });
      },
    },
    catalog,
    credentials,
    validator,
    callLog: log,
    quota,
    upstream,
    clock,
    publicUrl: PUBLIC_URL,
    maxResponseBytes: 20 * 1024 * 1024,
  };
});

const call = (principal: Principal, ref = 'gitea.create_issue', args = {}) =>
  callTool(deps, { principal, ref, arguments: args });

const text = (r: ToolCallResult) =>
  r.content
    .map((c) => (typeof c === 'object' && c !== null && 'text' in c ? String(c.text) : ''))
    .join('');
const outcomes = () => log.entries.map((e) => e.outcome);

describe('callTool: success and upstream results', () => {
  it('returns the upstream result as the same object, structuredContent and image included', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    const result: ToolCallResult = {
      content: [
        { type: 'text', text: 'created' },
        { type: 'image', data: 'iVBORw0KGgo=', mimeType: 'image/png' },
      ],
      structuredContent: { id: 42, labels: ['a'] },
    };
    upstream.script.push(result);
    const r = await call(ALICE, 'gitea.create_issue', { title: 'Bug' });
    expect(r).toBe(result);
    expect(r).toStrictEqual({
      content: [
        { type: 'text', text: 'created' },
        { type: 'image', data: 'iVBORw0KGgo=', mimeType: 'image/png' },
      ],
      structuredContent: { id: 42, labels: ['a'] },
    });
    expect(upstream.calls).toEqual([
      {
        target: {
          url: 'https://gitea.example.com/mcp',
          headers: {},
          timeoutMs: 30_000,
          maxResponseBytes: 20 * 1024 * 1024,
        },
        name: 'create_issue',
        args: { title: 'Bug' },
      },
    ]);
  });

  it('logs ok with exactly the call-log fields: no arguments, no result', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    const started = clock.current;
    upstream.callTool = (t, n, a) => {
      clock.current = new Date(started.getTime() + 1500);
      return FakeUpstream.prototype.callTool.call(upstream, t, n, a);
    };
    await call(ALICE, 'gitea.create_issue', { title: 'secret text' });
    expect(log.entries).toStrictEqual([
      {
        userId: 'alice',
        tokenId: 'tok-a',
        server: 'gitea',
        tool: 'create_issue',
        startedAt: started,
        durationMs: 1500,
        outcome: 'ok',
      },
    ]);
  });

  it('a session call (no token) logs tokenId null', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    await call({ userId: 'alice', role: 'member', groups: [] });
    expect(log.entries[0]?.tokenId).toBeNull();
  });

  it('upstream isError: true is passed on as is, logged upstream_error', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    const result: ToolCallResult = { content: [{ type: 'text', text: 'no repo' }], isError: true };
    upstream.script.push(result);
    expect(await call(ALICE)).toBe(result);
    expect(outcomes()).toEqual(['upstream_error']);
  });

  it('upstream tool names with dots are called by their own name (split on the first dot)', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    catalog.addTool('gitea', 'issues.create');
    await call(ALICE, 'gitea.issues.create');
    expect(upstream.calls[0]?.name).toBe('issues.create');
    expect(log.entries[0]).toMatchObject({ server: 'gitea', tool: 'issues.create' });
  });
});

const notFound = (ref: string) =>
  `Tool "${ref}" is not available. Use search_tools to find another.`;

async function expectNotFound(ref = 'gitea.create_issue') {
  const r = await call(ALICE, ref);
  expect(r).toStrictEqual({ content: [{ type: 'text', text: notFound(ref) }], isError: true });
  expect(upstream.calls).toHaveLength(0);
  expect(outcomes()).toEqual(['denied:not_found']);
}

describe('callTool: access re-checked on every call (Review Focus 3)', () => {
  beforeEach(() => {
    catalog.addServer('gitea', { kind: 'none' }, { groups: ['dev'] }, 5);
  });

  it('hidden by the admin after search', async () => {
    catalog.tools.get('gitea.create_issue')!.hidden = true;
    await expectNotFound();
  });

  it('gone from upstream after search', async () => {
    catalog.tools.get('gitea.create_issue')!.removed = true;
    await expectNotFound();
  });

  it('server disabled after search', async () => {
    catalog.servers.get('gitea')!.status = 'disabled';
    await expectNotFound();
  });

  it('person lost the group after search', async () => {
    catalog.servers.get('gitea')!.groups = ['ops'];
    await expectNotFound();
  });

  it('unknown tool, unknown server, malformed ref', async () => {
    await expectNotFound('gitea.nope');
    log.entries.length = 0;
    await expectNotFound('nowhere.create_issue');
    log.entries.length = 0;
    await expectNotFound('no-dot');
  });

  it('not_found spends no quota', async () => {
    catalog.servers.get('gitea')!.groups = ['ops'];
    await call(ALICE);
    expect(quota.counts.size).toBe(0);
  });

  it('the server disabled between the tool lookup and the server lookup is still not_found', async () => {
    const toolFor = catalog.toolFor.bind(catalog);
    catalog.toolFor = async (p, ref) => {
      const t = await toolFor(p, ref);
      catalog.servers.get('gitea')!.status = 'disabled';
      return t;
    };
    await expectNotFound();
  });
});

describe('callTool: arguments', () => {
  it('not matching the schema → path and hint, upstream not called, quota not spent', async () => {
    catalog.addServer('gitea', { kind: 'none' }, {}, 5);
    const r = await call(ALICE, 'gitea.create_issue', { title: 7 });
    expect(r).toStrictEqual({
      content: [
        {
          type: 'text',
          text: 'Invalid arguments at /title: must be string. Call describe_tool("gitea.create_issue") for the schema.',
        },
      ],
      isError: true,
    });
    expect(upstream.calls).toHaveLength(0);
    expect(quota.counts.size).toBe(0);
    expect(outcomes()).toEqual(['denied:invalid_arguments']);
  });
});

describe('callTool: credentials', () => {
  it('shared_secret: header from the template with the server secret', async () => {
    catalog.addServer(
      'gitea',
      { kind: 'shared_secret', header: 'X-API-Key', template: 'key {secret}' },
      { secret: 's3' },
    );
    await call(ALICE);
    expect(upstream.calls[0]?.target.headers).toEqual({ 'X-API-Key': 'key s3' });
  });

  it('shared_secret without a secret set → rejected text, upstream not called', async () => {
    catalog.addServer('gitea', { kind: 'shared_secret', ...BEARER });
    const r = await call(ALICE);
    expect(text(r)).toBe(
      'Gitea rejected the gateway credentials. Ask the administrator to check the server key.',
    );
    expect(r.isError).toBe(true);
    expect(upstream.calls).toHaveLength(0);
    expect(outcomes()).toEqual(['transport_error']);
  });

  it('pat_user and oauth_user: the person’s own secret; two people get their own headers', async () => {
    catalog.addServer('gitea', OAUTH);
    catalog.addServer('pats', { kind: 'pat_user', header: 'X-Token', template: 'token {secret}' });
    catalog.servers.get('pats')!.target.name = 'Pats';
    credentials.set('alice', 'gitea', 'oa-alice');
    credentials.set('bob', 'gitea', 'oa-bob');
    credentials.set('alice', 'pats', 'pat-alice');
    credentials.set('bob', 'pats', 'pat-bob');
    await call(ALICE);
    await call(BOB);
    await call(ALICE, 'pats.create_issue');
    await call(BOB, 'pats.create_issue');
    expect(upstream.calls.map((c) => c.target.headers)).toEqual([
      { Authorization: 'Bearer oa-alice' },
      { Authorization: 'Bearer oa-bob' },
      { 'X-Token': 'token pat-alice' },
      { 'X-Token': 'token pat-bob' },
    ]);
  });

  it('oauth_user without a connection → needs_connection with the link, before the quota', async () => {
    catalog.addServer('gitea', OAUTH, {}, 1);
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [
        {
          type: 'text',
          text: `Gitea is not connected. Ask the user to open ${PUBLIC_URL}/connections and connect Gitea.`,
        },
      ],
      isError: true,
    });
    expect(upstream.calls).toHaveLength(0);
    expect(quota.counts.size).toBe(0);
    expect(outcomes()).toEqual(['denied:needs_connection']);
  });

  it('an expired connection → expired text with the link', async () => {
    catalog.addServer('gitea', OAUTH);
    credentials.tokens.set('alice/gitea', err('expired'));
    const r = await call(ALICE);
    expect(text(r)).toBe(
      `Gitea connection expired. Ask the user to reconnect at ${PUBLIC_URL}/connections.`,
    );
    expect(outcomes()).toEqual(['denied:expired']);
    expect(upstream.calls).toHaveLength(0);
  });

  it('OAuth provider down (unavailable) → unavailable text, transport_error, upstream not called', async () => {
    catalog.addServer('gitea', OAUTH);
    credentials.tokens.set('alice/gitea', err('unavailable'));
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [{ type: 'text', text: 'Gitea is unavailable.' }],
      isError: true,
    });
    expect(outcomes()).toEqual(['transport_error']);
    expect(upstream.calls).toHaveLength(0);
  });
});

describe('callTool: upstream 401 (Review Focus 1)', () => {
  it('oauth_user: refreshes once and retries with the new header; quota spent once', async () => {
    catalog.addServer('gitea', OAUTH, {}, 5);
    credentials.set('alice', 'gitea', 'old');
    credentials.refreshTo.set('alice/gitea', ok('new'));
    const result: ToolCallResult = { content: [{ type: 'text', text: 'ok after refresh' }] };
    upstream.script.push(new UpstreamError('unauthorized'), result);
    const r = await call(ALICE);
    expect(r).toBe(result);
    expect(upstream.calls.map((c) => c.target.headers)).toEqual([
      { Authorization: 'Bearer old' },
      { Authorization: 'Bearer new' },
    ]);
    expect(credentials.resolved).toEqual([
      { userId: 'alice', server: 'gitea', forceRefresh: false },
      { userId: 'alice', server: 'gitea', forceRefresh: true },
    ]);
    expect([...quota.counts.values()]).toEqual([1]);
    expect(outcomes()).toEqual(['ok']);
  });

  it('oauth_user: a second 401 → expired text, denied:expired, the connection expires', async () => {
    catalog.addServer('gitea', OAUTH, {}, 5);
    credentials.set('alice', 'gitea', 'old');
    credentials.refreshTo.set('alice/gitea', ok('new'));
    upstream.script.push(new UpstreamError('unauthorized'), new UpstreamError('unauthorized'));
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [
        {
          type: 'text',
          text: `Gitea connection expired. Ask the user to reconnect at ${PUBLIC_URL}/connections.`,
        },
      ],
      isError: true,
    });
    expect(upstream.calls).toHaveLength(2);
    expect(credentials.expired).toEqual([{ userId: 'alice', server: 'gitea' }]);
    expect([...quota.counts.values()]).toEqual([1]);
    expect(outcomes()).toEqual(['denied:expired']);
  });

  it('oauth_user: refresh rejected by the provider → expired text, no retry', async () => {
    catalog.addServer('gitea', OAUTH);
    credentials.set('alice', 'gitea', 'old');
    upstream.script.push(new UpstreamError('unauthorized'));
    const r = await call(ALICE);
    expect(text(r)).toBe(
      `Gitea connection expired. Ask the user to reconnect at ${PUBLIC_URL}/connections.`,
    );
    expect(upstream.calls).toHaveLength(1);
    expect(outcomes()).toEqual(['denied:expired']);
  });

  it('oauth_user: provider down during the refresh → unavailable text, transport_error', async () => {
    catalog.addServer('gitea', OAUTH);
    credentials.set('alice', 'gitea', 'old');
    credentials.refreshTo.set('alice/gitea', err('unavailable'));
    upstream.script.push(new UpstreamError('unauthorized'));
    expect(text(await call(ALICE))).toBe('Gitea is unavailable.');
    expect(upstream.calls).toHaveLength(1);
    expect(outcomes()).toEqual(['transport_error']);
  });

  it('oauth_user: the retry times out → timeout text', async () => {
    catalog.addServer('gitea', OAUTH);
    credentials.set('alice', 'gitea', 'old');
    credentials.refreshTo.set('alice/gitea', ok('new'));
    upstream.script.push(new UpstreamError('unauthorized'), new UpstreamError('timeout'));
    expect(text(await call(ALICE))).toBe('Gitea did not respond in 30 s.');
    expect(outcomes()).toEqual(['timeout']);
  });

  it('pat_user: marks the connection expired, expired text, no retry', async () => {
    catalog.addServer('gitea', { kind: 'pat_user', ...BEARER });
    credentials.set('alice', 'gitea', 'pat');
    upstream.script.push(new UpstreamError('unauthorized'));
    const r = await call(ALICE);
    expect(text(r)).toBe(
      `Gitea connection expired. Ask the user to reconnect at ${PUBLIC_URL}/connections.`,
    );
    expect(credentials.expired).toEqual([{ userId: 'alice', server: 'gitea' }]);
    expect(credentials.resolved.every((q) => !q.forceRefresh)).toBe(true);
    expect(upstream.calls).toHaveLength(1);
    expect(outcomes()).toEqual(['denied:expired']);
  });

  it('shared_secret: rejected text, transport_error, no retry', async () => {
    catalog.addServer('gitea', { kind: 'shared_secret', ...BEARER }, { secret: 'k' });
    upstream.script.push(new UpstreamError('unauthorized'));
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [
        {
          type: 'text',
          text: 'Gitea rejected the gateway credentials. Ask the administrator to check the server key.',
        },
      ],
      isError: true,
    });
    expect(upstream.calls).toHaveLength(1);
    expect(outcomes()).toEqual(['transport_error']);
  });

  it('none: rejected text as well', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    upstream.script.push(new UpstreamError('unauthorized'));
    expect(text(await call(ALICE))).toContain('rejected the gateway credentials');
    expect(outcomes()).toEqual(['transport_error']);
  });
});

describe('callTool: daily limit', () => {
  it('dailyLimit 2: the third call of the day → quota with the next UTC midnight; the next day passes', async () => {
    catalog.addServer('gitea', { kind: 'none' }, {}, 2);
    clock.current = new Date('2026-10-30T23:59:00Z');
    await call(ALICE);
    await call(ALICE);
    const third = await call(ALICE);
    expect(third).toStrictEqual({
      content: [
        {
          type: 'text',
          text: 'Daily limit for Gitea reached. Resets at 2026-10-31T00:00:00.000Z.',
        },
      ],
      isError: true,
    });
    expect(upstream.calls).toHaveLength(2);
    expect(await call(BOB)).not.toHaveProperty('isError');
    clock.current = new Date('2026-10-31T00:00:00Z');
    expect((await call(ALICE)).isError).toBeUndefined();
    expect(outcomes()).toEqual(['ok', 'ok', 'denied:quota', 'ok', 'ok']);
  });

  it('no dailyLimit: the quota is not consulted', async () => {
    catalog.addServer('gitea', { kind: 'none' });
    await Promise.all(Array.from({ length: 5 }, () => call(ALICE)));
    expect(quota.counts.size).toBe(0);
    expect(upstream.calls).toHaveLength(5);
  });
});

describe('callTool: upstream failures (Review Focus 4)', () => {
  beforeEach(() => {
    catalog.addServer('gitea', { kind: 'none' });
  });

  it('timeout → short text with the server timeout, logged timeout', async () => {
    upstream.script.push(new UpstreamError('timeout'));
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [{ type: 'text', text: 'Gitea did not respond in 30 s.' }],
      isError: true,
    });
    expect(outcomes()).toEqual(['timeout']);
  });

  it('too large → the limit in MiB, logged transport_error', async () => {
    upstream.script.push(new UpstreamError('too_large'));
    expect(text(await call(ALICE))).toBe('Gitea returned a response larger than 20 MiB.');
    expect(outcomes()).toEqual(['transport_error']);
  });

  it('transport → unavailable, no internal details', async () => {
    upstream.script.push(new UpstreamError('transport', 'ECONNREFUSED 10.0.0.1:443'));
    const r = await call(ALICE);
    expect(text(r)).toBe('Gitea is unavailable.');
    expect(outcomes()).toEqual(['transport_error']);
  });

  it('a JSON-RPC error (invalid params): code and message for the agent, logged upstream_error', async () => {
    upstream.script.push(UpstreamError.rejected(-32602, 'Invalid params: owner is required'));
    const r = await call(ALICE);
    expect(r).toStrictEqual({
      content: [
        {
          type: 'text',
          text: 'Gitea rejected the call (JSON-RPC -32602): Invalid params: owner is required',
        },
      ],
      isError: true,
    });
    expect(outcomes()).toEqual(['upstream_error']);
  });

  it('a JSON-RPC internal error is passed on the same way; a long message is cut', async () => {
    upstream.script.push(UpstreamError.rejected(-32603, `failed: ${'x'.repeat(5000)}`));
    const t = text(await call(ALICE));
    expect(t.startsWith('Gitea rejected the call (JSON-RPC -32603): failed: xxx')).toBe(true);
    expect(t.length).toBeLessThanOrEqual(600);
    expect(t.endsWith('…')).toBe(true);
    expect(outcomes()).toEqual(['upstream_error']);
  });

  it('a JSON-RPC error without a message: the code alone', async () => {
    upstream.script.push(UpstreamError.rejected(-32602, '  '));
    expect(text(await call(ALICE))).toBe('Gitea rejected the call (JSON-RPC -32602).');
  });

  it('an unexpected error is logged transport_error and rethrown', async () => {
    upstream.callTool = () => Promise.reject(new Error('bug'));
    await expect(call(ALICE)).rejects.toThrow('bug');
    expect(outcomes()).toEqual(['transport_error']);
  });
});

describe('callTool: the call log never changes the answer', () => {
  beforeEach(() => {
    catalog.addServer('gitea', { kind: 'none' });
    log.record = () => Promise.reject(new Error('pool exhausted'));
  });

  it('a failing record is logged at error level (no arguments) and the result still returned', async () => {
    const result: ToolCallResult = { content: [{ type: 'text', text: 'done' }] };
    upstream.script.push(result);
    expect(await call(ALICE, 'gitea.create_issue', { title: 'private words' })).toBe(result);
    expect(errors).toHaveLength(1);
    expect(errors[0]?.message).toBe('gateway: call log record failed');
    expect(errors[0]?.fields).toMatchObject({
      server: 'gitea',
      tool: 'create_issue',
      outcome: 'ok',
    });
    expect(JSON.stringify(errors)).not.toContain('private words');
  });

  it('a JSON-RPC error message from upstream is never logged', async () => {
    upstream.script.push(UpstreamError.rejected(-32602, 'owner "private-words" not found'));
    await call(ALICE, 'gitea.create_issue', { title: 'private words' });
    expect(errors).toHaveLength(1);
    expect(errors[0]?.fields).toMatchObject({ outcome: 'upstream_error' });
    expect(JSON.stringify(errors)).not.toContain('private');
  });

  it('a failing record does not hide the original error', async () => {
    upstream.callTool = () => Promise.reject(new Error('bug'));
    await expect(call(ALICE)).rejects.toThrow('bug');
    expect(errors).toHaveLength(1);
  });
});

describe('callTool: agent text in the call log is bounded', () => {
  it('server and tool are cut to 200 characters', async () => {
    const longTool = `t${'x'.repeat(500)}`;
    await call(ALICE, `gitea.${longTool}`);
    await call(ALICE, 'y'.repeat(500));
    expect(log.entries.map((e) => [e.server.length, e.tool.length])).toEqual([
      [5, 200],
      [0, 200],
    ]);
    expect(outcomes()).toEqual(['denied:not_found', 'denied:not_found']);
  });
});
