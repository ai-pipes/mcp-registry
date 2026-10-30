import { request } from 'node:http';
import { type ServerType, serve } from '@hono/node-server';
import { Client, StreamableHTTPClientTransport } from '@modelcontextprotocol/client';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { z } from 'zod';
import {
  type AuthMode,
  applyToolSnapshot,
  createServer,
  type ToolSnapshot,
  syncServer,
} from '../../src/modules/catalog/index.ts';
import { savePat } from '../../src/modules/connections/index.ts';
import { createToken, type Principal, setDisabled } from '../../src/modules/identity/index.ts';
import { reindexAll } from '../../src/modules/search/index.ts';
import { loadConfig } from '../../src/platform/config.ts';
import { buildContainer, type Container } from '../../src/platform/container.ts';
import { createHttpApp } from '../../src/platform/http/app.ts';
import { createLogger } from '../../src/platform/logger.ts';
import { SERVER_INSTRUCTIONS } from '../../src/platform/mcp/instructions.ts';
import { type FakeUpstream, startFakeUpstream } from '../fake-upstream.ts';
import { FakeEmbedder } from '../fakes/fake-embedder.ts';
import { startPg, truncateAll } from '../pg.ts';

const PUBLIC_URL = 'http://mcp.test';
const LONG_DESCRIPTION =
  'Create an issue in a YouTrack project. ' +
  'The summary is required; the description supports Markdown, mentions and attachments that ' +
  'were uploaded before. Custom fields are set by name, for example Priority, Type or Assignee, ' +
  'and unknown names are rejected by the tracker.';

let pg: Awaited<ReturnType<typeof startPg>>;
let gitea: FakeUpstream;
let youtrack: FakeUpstream;
let c: Container;
let http: { close: () => Promise<void> };
let server: ServerType;
let baseUrl: string;
let clients: Client[];

const snapshot = (name: string, description: string, extra: Partial<ToolSnapshot> = {}) =>
  ({
    name,
    title: null,
    description,
    inputSchema: { type: 'object', properties: { summary: { type: 'string' } } },
    outputSchema: null,
    annotations: {},
    ...extra,
  }) satisfies ToolSnapshot;

beforeAll(async () => {
  [pg, gitea, youtrack] = await Promise.all([
    startPg(),
    startFakeUpstream([
      { name: 'create_issue', description: 'Create an issue in a Gitea repository' },
      { name: 'get_file_contents', description: 'Read a file from a Gitea repository' },
    ]),
    startFakeUpstream(),
  ]);
  const config = loadConfig({
    DATABASE_URL: pg.url,
    PUBLIC_URL,
    SECRETS_KEY: Buffer.alloc(32, 7).toString('base64'),
    OIDC_ISSUER: 'https://id.example.com',
    OIDC_CLIENT_ID: 'x',
    OIDC_CLIENT_SECRET: 'y',
    // The fake embedder is a bag of tokens: a low floor keeps word overlap meaningful.
    SEARCH_MIN_SIMILARITY: '0.2',
    LOG_LEVEL: 'silent',
  });
  c = buildContainer(config, pg.sql, {
    embedder: new FakeEmbedder(),
    logger: createLogger('silent'),
  });
  const built = createHttpApp(c, { remoteAddress: () => '192.0.2.1', webRoot: '/nonexistent' });
  http = built;
  await new Promise<void>((resolve) => {
    server = serve({ fetch: built.app.fetch, port: 0, hostname: '127.0.0.1' }, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('no TCP address');
  baseUrl = `http://127.0.0.1:${address.port}`;
});

afterAll(async () => {
  await http.close();
  await new Promise<void>((resolve) => {
    server.close(() => resolve());
  });
  await Promise.all([pg.stop(), gitea.close(), youtrack.close()]);
});

let admin: Principal;
let alice: Principal;
let carol: Principal;
let bob: Principal;

async function user(sub: string, groups: string[], role: 'admin' | 'member' = 'member') {
  const u = await c.identity.users.upsertFromLogin({
    claims: { sub, name: sub, email: null, groups },
    role,
    at: new Date(),
  });
  return { userId: u.id, name: sub, role, groups, via: 'session' } satisfies Principal;
}

async function addServer(
  slug: string,
  name: string,
  url: string,
  auth: AuthMode,
  groups?: string[],
) {
  const r = await createServer(c.catalog, {
    principal: admin,
    slug,
    name,
    description: `${name} for the team`,
    url,
    auth,
    access: groups ? { kind: 'groups', groups } : { kind: 'all' },
    timeoutMs: 5000,
  });
  if (!r.ok) throw r.error;
}

beforeEach(async () => {
  await truncateAll(pg.sql);
  clients = [];
  for (const up of [gitea, youtrack]) up.requests.length = 0;
  admin = await user('admin', ['mcp-admins'], 'admin');
  alice = await user('alice', ['mcp-users', 'dev']);
  carol = await user('carol', ['mcp-users', 'dev']);
  bob = await user('bob', ['mcp-users']);

  await addServer('gitea', 'Gitea', gitea.url, { kind: 'none' });
  const synced = await syncServer(c.catalogSync, { principal: admin, slug: 'gitea' });
  if (!synced.ok) throw synced.error;
  await addServer(
    'youtrack',
    'YouTrack',
    youtrack.url,
    { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
    ['dev'],
  );
  await applyToolSnapshot(c.catalog, {
    server: 'youtrack',
    tools: [
      snapshot('create_issue', LONG_DESCRIPTION, {
        title: 'Create issue',
        inputSchema: {
          type: 'object',
          properties: { project: { type: 'string' }, summary: { type: 'string' } },
          required: ['project', 'summary'],
        },
        annotations: { readOnlyHint: false, destructiveHint: false },
      }),
      snapshot('search_issues', 'Find YouTrack issues with a query', {
        annotations: { readOnlyHint: true },
      }),
    ],
  });
  await reindexAll(c.search);
  const saved = await Promise.all([
    savePat(c.connections, { principal: alice, server: 'youtrack', token: 'alice-pat' }),
    savePat(c.connections, { principal: carol, server: 'youtrack', token: 'carol-pat' }),
  ]);
  for (const r of saved) if (!r.ok) throw r.error;
});

afterEach(async () => {
  await Promise.all(clients.map((cl) => cl.close()));
});

async function tokenFor(p: Principal): Promise<string> {
  return (await createToken(c.identity, { principal: p, name: 'cli' })).token;
}

async function connect(p: Principal): Promise<Client> {
  const token = await tokenFor(p);
  const client = new Client({ name: 'test-agent', version: '1.0.0' });
  await client.connect(
    new StreamableHTTPClientTransport(new URL(`${baseUrl}/mcp`), {
      requestInit: { headers: { authorization: `Bearer ${token}` } },
    }),
  );
  clients.push(client);
  return client;
}

type Json = Record<string, unknown>;
const JsonObject = z.record(z.string(), z.unknown());
const list = (v: unknown): Json[] => z.array(JsonObject).parse(v);

async function call(client: Client, name: string, args: Json = {}) {
  return client.callTool({ name, arguments: args });
}

async function structured(client: Client, name: string, args: Json = {}): Promise<Json> {
  const r = await call(client, name, args);
  expect(r.isError ?? false).toBe(false);
  return JsonObject.parse(r.structuredContent);
}

const errorText = (r: { content?: unknown; isError?: unknown }) => {
  expect(r.isError).toBe(true);
  return z.array(z.object({ text: z.string() })).parse(r.content)[0]?.text;
};

const NOT_FOUND = (ref: string) =>
  `Tool "${ref}" is not available. Use search_tools to find another.`;

const rpc = (headers: Record<string, string>) =>
  fetch(`${baseUrl}/mcp`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      accept: 'application/json, text/event-stream',
      ...headers,
    },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }),
  });

const youtrackCalls = () => youtrack.requests.filter((r) => r.method === 'tools/call');

describe('/mcp: the agent sees four meta-tools', () => {
  it('lists exactly four tools, sends the instructions, and does not grow with servers', async () => {
    const client = await connect(alice);
    const first = await client.listTools();
    expect(first.tools.map((t) => t.name).toSorted()).toEqual([
      'call_tool',
      'describe_tool',
      'list_servers',
      'search_tools',
    ]);
    expect(client.getInstructions()).toBe(SERVER_INSTRUCTIONS);
    expect(client.getServerCapabilities()?.tools?.listChanged).not.toBe(true);
    const size = JSON.stringify(first).length + SERVER_INSTRUCTIONS.length;
    expect(size, `tools/list + instructions: ${size} characters`).toBeLessThan(8000);
    expect(first.tools.find((t) => t.name === 'call_tool')?.annotations?.readOnlyHint).toBe(false);

    await addServer('extra', 'Extra', gitea.url, { kind: 'none' });
    const synced = await syncServer(c.catalogSync, { principal: admin, slug: 'extra' });
    expect(synced.ok).toBe(true);
    await reindexAll(c.search);

    const again = await (await connect(alice)).listTools();
    expect(again).toEqual(first);
    expect(await client.listTools()).toEqual(first);
  });

  it('list_servers: accessible servers with tool counts and connection state', async () => {
    const forAlice = await structured(await connect(alice), 'list_servers');
    expect(forAlice).toEqual({
      servers: [
        {
          slug: 'gitea',
          name: 'Gitea',
          description: 'Gitea for the team',
          tools: 2,
          connection: 'not_required',
        },
        {
          slug: 'youtrack',
          name: 'YouTrack',
          description: 'YouTrack for the team',
          tools: 2,
          connection: 'ready',
        },
      ],
    });
    // No URL, auth config or secret ever leaves through MCP.
    expect(JSON.stringify(forAlice)).not.toContain('127.0.0.1');

    const devWithoutPat = await user('dave', ['mcp-users', 'dev']);
    const forDave = await structured(await connect(devWithoutPat), 'list_servers');
    expect(forDave['servers']).toContainEqual({
      slug: 'youtrack',
      name: 'YouTrack',
      description: 'YouTrack for the team',
      tools: 2,
      connection: 'needs_connection',
      connectUrl: `${PUBLIC_URL}/connections`,
    });
  });

  it('success scenario: search → describe → call with the token of this very user', async () => {
    const client = await connect(alice);
    const found = await structured(client, 'search_tools', { query: 'create an issue' });
    const results = list(found['results']);
    const hit = results.find((r) => r['tool'] === 'youtrack.create_issue');
    expect(hit).toMatchObject({
      tool: 'youtrack.create_issue',
      title: 'Create issue',
      server: 'youtrack',
      annotations: { readOnlyHint: false, destructiveHint: false },
      connection: 'ready',
    });
    const short = String(hit?.['description']);
    expect(short.length).toBeLessThanOrEqual(200);
    expect(short.endsWith('…')).toBe(true);
    expect(LONG_DESCRIPTION.startsWith(short.slice(0, -1))).toBe(true);
    expect(typeof hit?.['score']).toBe('number');

    const described = await structured(client, 'describe_tool', { tool: 'youtrack.create_issue' });
    expect(described).toEqual({
      tool: 'youtrack.create_issue',
      title: 'Create issue',
      description: LONG_DESCRIPTION,
      inputSchema: {
        type: 'object',
        properties: { project: { type: 'string' }, summary: { type: 'string' } },
        required: ['project', 'summary'],
      },
      annotations: { readOnlyHint: false, destructiveHint: false },
      server: 'youtrack',
      connection: 'ready',
    });

    const args = { project: 'OPS', summary: 'Disk is full' };
    const result = await call(client, 'call_tool', {
      tool: 'youtrack.create_issue',
      arguments: args,
    });
    expect(result).toEqual({
      content: [{ type: 'text', text: JSON.stringify(args) }],
      structuredContent: { echo: args },
    });
    const calls = youtrackCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0]?.headers['authorization']).toBe('Bearer alice-pat');

    // Carol's token reaches upstream with Carol's credentials, not Alice's.
    await call(await connect(carol), 'call_tool', {
      tool: 'youtrack.create_issue',
      arguments: args,
    });
    expect(youtrackCalls()[1]?.headers['authorization']).toBe('Bearer carol-pat');
  });

  it('/mcp takes bodies above the 1 MiB REST limit (MCP_MAX_BODY_BYTES, 10 MiB) (M1)', async () => {
    const client = await connect(alice);
    // 5 MiB of arguments: above the REST limit and above the SDK's own 4 MiB default.
    const summary = 'x'.repeat(5 * 1024 * 1024);
    const result = await call(client, 'call_tool', {
      tool: 'youtrack.create_issue',
      arguments: { project: 'OPS', summary },
    });
    expect(result.isError).toBeUndefined();
    expect(youtrackCalls()).toHaveLength(1);

    // Above the /mcp limit: 413 before anything is parsed.
    const token = await tokenFor(alice);
    const res = await fetch(`${baseUrl}/mcp`, {
      method: 'POST',
      headers: {
        authorization: `Bearer ${token}`,
        'content-type': 'application/json',
        accept: 'application/json, text/event-stream',
      },
      body: 'x'.repeat(10 * 1024 * 1024 + 1),
    });
    expect(res.status).toBe(413);
  });

  it('search_tools: limit, unknown server and an empty answer', async () => {
    const client = await connect(alice);
    const one = await structured(client, 'search_tools', { query: 'issue', limit: 1 });
    expect(one['results']).toHaveLength(1);

    const onlyGitea = await structured(client, 'search_tools', {
      query: 'create an issue',
      servers: ['gitea'],
    });
    expect(list(onlyGitea['results']).map((r) => r['server'])).toEqual(['gitea']);

    expect(
      errorText(await call(client, 'search_tools', { query: 'issue', servers: ['jira'] })),
    ).toBe('Unknown server "jira". Call list_servers.');

    const tooMany = await call(client, 'search_tools', { query: 'issue', limit: 26 });
    expect(tooMany.isError).toBe(true);

    expect(await structured(client, 'search_tools', { query: 'zzzqqq' })).toEqual({ results: [] });
  });

  it('describe_tool and call_tool on an unknown tool → not found, upstream untouched', async () => {
    const client = await connect(alice);
    expect(errorText(await call(client, 'describe_tool', { tool: 'youtrack.nope' }))).toBe(
      NOT_FOUND('youtrack.nope'),
    );
    expect(errorText(await call(client, 'describe_tool', { tool: 'no-dot' }))).toBe(
      NOT_FOUND('no-dot'),
    );
    expect(
      errorText(await call(client, 'call_tool', { tool: 'youtrack.nope', arguments: {} })),
    ).toBe(NOT_FOUND('youtrack.nope'));
    expect(youtrackCalls()).toHaveLength(0);
  });
});

describe('/mcp: access', () => {
  it('a person without access to youtrack neither sees nor calls it', async () => {
    const client = await connect(bob);
    const servers = await structured(client, 'list_servers');
    expect(list(servers['servers']).map((s) => s['slug'])).toEqual(['gitea']);

    const found = await structured(client, 'search_tools', { query: 'create an issue' });
    const results = list(found['results']);
    expect(results.length).toBeGreaterThan(0);
    expect(results.every((r) => r['server'] === 'gitea')).toBe(true);
    expect(
      errorText(await call(client, 'search_tools', { query: 'issue', servers: ['youtrack'] })),
    ).toBe('Unknown server "youtrack". Call list_servers.');

    expect(errorText(await call(client, 'describe_tool', { tool: 'youtrack.create_issue' }))).toBe(
      NOT_FOUND('youtrack.create_issue'),
    );
    expect(
      errorText(
        await call(client, 'call_tool', {
          tool: 'youtrack.create_issue',
          arguments: { project: 'OPS', summary: 'x' },
        }),
      ),
    ).toBe(NOT_FOUND('youtrack.create_issue'));
    expect(youtrackCalls()).toHaveLength(0);
  });

  it('without Authorization → 401 with WWW-Authenticate: Bearer', async () => {
    const res = await rpc({});
    expect(res.status).toBe(401);
    expect(res.headers.get('www-authenticate')).toBe('Bearer');
  });

  it('a disabled person’s token → 401', async () => {
    const token = await tokenFor(alice);
    const off = await setDisabled(c.identity, {
      principal: admin,
      userId: alice.userId,
      disabled: true,
    });
    expect(off.ok).toBe(true);
    const res = await rpc({ authorization: `Bearer ${token}` });
    expect(res.status).toBe(401);
    expect(res.headers.get('www-authenticate')).toBe('Bearer');
  });

  it('a foreign Origin or Host is refused before the token is looked at', async () => {
    const token = await tokenFor(alice);
    const foreign = await rpc({ authorization: `Bearer ${token}`, origin: 'http://evil.test' });
    expect(foreign.status).toBe(403);
    // fetch always sends the real Host; node:http lets the test forge it.
    const status = await new Promise<number | undefined>((resolve, reject) => {
      const req = request(
        `${baseUrl}/mcp`,
        {
          method: 'POST',
          headers: {
            host: 'evil.test',
            authorization: `Bearer ${token}`,
            'content-type': 'application/json',
            accept: 'application/json, text/event-stream',
          },
        },
        (res) => {
          res.resume();
          resolve(res.statusCode);
        },
      );
      req.on('error', reject);
      req.end(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }));
    });
    expect(status).toBe(403);
  });
});
