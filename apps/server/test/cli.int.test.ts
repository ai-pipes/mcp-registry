import { randomBytes } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { DEFAULT_MIN_SIMILARITY } from '../src/modules/search/index.ts';
import { getServerSecret } from '../src/modules/catalog/index.ts';
import { Connection } from '../src/modules/connections/domain/connection.ts';
import { authenticateToken } from '../src/modules/identity/index.ts';
import { runCli } from '../src/platform/cli-commands.ts';
import type { Config } from '../src/platform/config.ts';
import { buildContainer, type Container } from '../src/platform/container.ts';
import { AesGcmSecretBox } from '../src/platform/crypto/secret-box.ts';
import { createLogger } from '../src/platform/logger.ts';
import { type FakeUpstream, startFakeUpstream } from './fake-upstream.ts';
import { FakeEmbedder } from './fakes/fake-embedder.ts';
import { startPg, truncateAll } from './pg.ts';

const KEY = new Uint8Array(randomBytes(32));
const SECRET = 'upstream-shared-key-Zq81';

let pg: Awaited<ReturnType<typeof startPg>>;
let up: FakeUpstream;
let dir: string;

function configFor(over: Partial<Config> = {}): Config {
  return {
    port: 3000,
    databaseUrl: 'postgres://unused',
    publicUrl: 'https://mcp.example.com/',
    allowedHosts: [],
    trustProxy: false,
    trustProxyHops: 1,
    cookieSecure: true,
    secretsKey: KEY,
    oidc: {
      issuer: 'https://id.example.com',
      clientId: 'id',
      clientSecret: 'secret',
      loginGroup: 'mcp-users',
      adminGroup: 'mcp-admins',
    },
    syncIntervalHours: 6,
    callLogRetentionDays: 90,
    upstreamMaxResponseBytes: 1024 * 1024,
    mcpMaxBodyBytes: 10 * 1024 * 1024,
    embedder: 'local',
    modelDir: '/nonexistent',
    searchMinSimilarity: DEFAULT_MIN_SIMILARITY,
    logLevel: 'silent',
    ...over,
  };
}

const containerFor = (over: Partial<Config> = {}): Container =>
  buildContainer(configFor(over), pg.sql, {
    embedder: new FakeEmbedder(),
    logger: createLogger('silent'),
  });

async function cli(
  argv: string[],
  opts: { stdin?: string; env?: Record<string, string>; container?: Container } = {},
) {
  const out: string[] = [];
  const err: string[] = [];
  let stdinRead = false;
  const code = await runCli(argv, opts.container ?? containerFor(), {
    readStdin: () => {
      stdinRead = true;
      return Promise.resolve(opts.stdin ?? '');
    },
    env: (name) => opts.env?.[name],
    out: (line) => out.push(line),
    err: (line) => err.push(line),
  });
  return { code, out, err, all: [...out, ...err].join('\n'), stdinRead };
}

async function serverJson(over: Record<string, unknown> = {}): Promise<string> {
  const file = join(dir, `server-${Math.random().toString(36).slice(2)}.json`);
  await writeFile(
    file,
    JSON.stringify({
      slug: 'fake',
      name: 'Fake',
      description: 'Fake upstream',
      url: up.url,
      auth: { kind: 'shared_secret', header: 'X-Api-Key', template: 'Key {secret}' },
      access: { kind: 'all' },
      ...over,
    }),
  );
  return file;
}

async function admin(sub = 'owner') {
  const r = await cli(['create-user', '--sub', sub, '--name', 'Owner', '--admin']);
  expect(r.code).toBe(0);
}

beforeAll(async () => {
  [pg, up] = await Promise.all([startPg(), startFakeUpstream()]);
  dir = await mkdtemp(join(tmpdir(), 'mcp-cli-'));
});
afterAll(async () => {
  await Promise.all([pg.stop(), up.close(), rm(dir, { recursive: true, force: true })]);
});
beforeEach(async () => {
  await truncateAll(pg.sql);
  up.setTools([{ name: 'echo', description: 'Echo the arguments back' }]);
  up.requests.length = 0;
});

describe('cli users and tokens', () => {
  it('create-user is idempotent by sub', async () => {
    const a = await cli(['create-user', '--sub', 'bob', '--name', 'Bob']);
    const b = await cli(['create-user', '--sub', 'bob', '--name', 'Bob']);
    expect([a.code, b.code]).toEqual([0, 0]);
    expect(a.out[0]).toMatch(/^created member "Bob" \(sub bob, id [0-9a-f-]{36}\)$/);
    expect(b.out[0]).toBe(a.out[0]?.replace('created', 'updated'));
    const rows = await pg.sql`select role from identity.users where oidc_sub = 'bob'`;
    expect(rows).toEqual([{ role: 'member' }]);
    const promoted = await cli(['create-user', '--sub', 'bob', '--name', 'Bob', '--admin']);
    expect(promoted.out[0]).toMatch(/^updated admin "Bob"/);
  });

  it('create-token prints an sk_ token and the claude mcp add command; the token works', async () => {
    await admin();
    const r = await cli(['create-token', 'owner', '--name', 'laptop']);
    expect(r.code).toBe(0);
    const [token, command] = r.out;
    expect(token).toMatch(/^sk_/);
    expect(command).toBe(
      `claude mcp add --transport http mcp-registry https://mcp.example.com/mcp --header "Authorization: Bearer ${token}"`,
    );
    const auth = await authenticateToken(containerFor().identity, { token: token ?? '', ip: 'x' });
    expect(auth).toMatchObject({ ok: true, value: { name: 'Owner', via: 'token' } });
    expect((await cli(['create-token', 'ghost', '--name', 'x'])).code).toBe(1);
  });

  it('prints usage for an unknown command or bad arguments', async () => {
    const r = await cli(['frobnicate']);
    expect(r.code).toBe(2);
    expect(r.err.join('\n')).toContain('Usage');
    expect((await cli([])).code).toBe(2);
    expect((await cli(['create-token', 'owner'])).code).toBe(2);
    expect((await cli(['create-user', '--sub', 'x'])).code).toBe(2);
    expect((await cli(['reindex', 'extra'])).code).toBe(2);
    expect((await cli(['create-server', '--json', 'x.json'])).code).toBe(2);
  });
});

describe('cli servers', () => {
  it('create-server takes the secret from --secret-env, never prints it, and syncs', async () => {
    await admin();
    const file = await serverJson();
    const r = await cli(['create-server', '--json', file, '--as', 'owner', '--secret-env', 'K'], {
      env: { K: SECRET },
    });
    expect(r.code).toBe(0);
    expect(r.stdinRead).toBe(false);
    expect(r.all).not.toContain(SECRET);
    expect(r.out).toEqual(['created server "fake" (shared_secret, secret set); 1 tool(s)']);
    const c = containerFor();
    expect(await getServerSecret(c.catalog, 'fake')).toBe(SECRET);
    const sent = up.requests.find((q) => q.method === 'tools/list');
    expect(sent?.headers['x-api-key']).toBe(`Key ${SECRET}`);
  });

  it('create-server reads the secret from stdin (one trailing newline dropped)', async () => {
    await admin();
    const r = await cli(['create-server', '--json', await serverJson(), '--as', 'owner'], {
      stdin: `${SECRET}\n`,
    });
    expect(r.code).toBe(0);
    expect(r.stdinRead).toBe(true);
    expect(r.all).not.toContain(SECRET);
    expect(await getServerSecret(containerFor().catalog, 'fake')).toBe(SECRET);
  });

  it('create-server does not read stdin for a mode without a secret', async () => {
    await admin();
    const file = await serverJson({ auth: { kind: 'none' } });
    const r = await cli(['create-server', '--json', file, '--as', 'owner']);
    expect(r.code).toBe(0);
    expect(r.stdinRead).toBe(false);
    expect(r.out).toEqual(['created server "fake" (none); 1 tool(s)']);
  });

  it('create-server refuses invalid JSON input, an unset --secret-env and a non-admin', async () => {
    await admin();
    const bad = await cli([
      'create-server',
      '--json',
      await serverJson({ url: 'ftp://x' }),
      '--as',
      'owner',
    ]);
    expect(bad.code).toBe(1);
    expect(bad.err.join('\n')).toContain('url');
    const unset = await cli(
      ['create-server', '--json', await serverJson(), '--as', 'owner', '--secret-env', 'NOPE'],
      { env: {} },
    );
    expect(unset.code).toBe(2);
    await cli(['create-user', '--sub', 'm', '--name', 'M']);
    const member = await cli(['create-server', '--json', await serverJson(), '--as', 'm'], {
      stdin: SECRET,
    });
    expect(member.code).toBe(1);
    expect(member.err.join('\n')).toContain('forbidden');
    const missing = await cli([
      'create-server',
      '--json',
      join(dir, 'missing.json'),
      '--as',
      'owner',
    ]);
    expect(missing.code).toBe(1);
  });

  it('a failed first sync is reported, but the server is created', async () => {
    await admin();
    up.status(500);
    try {
      const r = await cli(['create-server', '--json', await serverJson(), '--as', 'owner'], {
        stdin: SECRET,
      });
      expect(r.code).toBe(0);
      expect(r.out[0]).toMatch(
        /^created server "fake" \(shared_secret, secret set\); sync failed: /,
      );
    } finally {
      up.reset();
    }
  });

  it('sync re-reads the tools, as an admin or as the scheduler', async () => {
    await admin();
    await cli(['create-server', '--json', await serverJson(), '--as', 'owner'], { stdin: SECRET });
    up.setTools([{ name: 'echo' }, { name: 'issues.create' }]);
    const r = await cli(['sync', 'fake', '--as', 'owner']);
    expect(r.code).toBe(0);
    expect(r.out).toEqual(['synced "fake": 1 added, 1 changed, 0 removed']);
    expect((await cli(['sync', 'fake'])).out).toEqual([
      'synced "fake": 0 added, 0 changed, 0 removed',
    ]);
    const unknown = await cli(['sync', 'nope']);
    expect(unknown.code).toBe(1);
  });

  it('reindex rebuilds the search index and reports the server count', async () => {
    await admin();
    await cli(['create-server', '--json', await serverJson(), '--as', 'owner'], { stdin: SECRET });
    const r = await cli(['reindex']);
    expect(r.code).toBe(0);
    expect(r.out).toEqual(['reindexed 1 server(s)']);
    const docs = await pg.sql`select ref from search.documents`;
    expect(docs).toEqual([{ ref: 'fake.echo' }]);
  });
});

/** Every sealed value in the database, whichever module owns it. */
async function sealedValues(): Promise<Uint8Array[]> {
  const servers = await pg.sql<{ secret: Buffer | null }[]>`select secret from catalog.servers`;
  const conns = await pg.sql<
    { access: Buffer; refresh: Buffer | null }[]
  >`select access, refresh from connections.connections`;
  return [...servers.map((r) => r.secret), ...conns.flatMap((r) => [r.access, r.refresh])].flatMap(
    (b) => (b ? [new Uint8Array(b)] : []),
  );
}

describe('cli rotate-secrets-key', () => {
  const OLD = KEY;
  const NEW = new Uint8Array(randomBytes(32));

  async function seedUnderOldKey() {
    await admin();
    await cli(['create-server', '--json', await serverJson(), '--as', 'owner'], { stdin: SECRET });
    const c = containerFor();
    const [user] = await pg.sql<{ id: string }[]>`select id from identity.users`;
    const box = new AesGcmSecretBox(OLD);
    const now = new Date();
    const seed = (connection: Connection) =>
      c.connections.connections.withLock(connection.userId, connection.server, (_c, save) =>
        save(connection),
      );
    await seed(
      Connection.connectOAuth({
        userId: user!.id,
        server: 'gitea',
        access: box.seal('access-token-1'),
        refresh: box.seal('refresh-token-1'),
        expiresAt: new Date(now.getTime() + 3_600_000),
        issuedFor: 'fp',
        revokeUrl: null,
        now,
      }),
    );
    await seed(
      Connection.connectPat({
        userId: user!.id,
        server: 'youtrack',
        token: box.seal('pat-token-1'),
        issuedFor: 'fp',
        now,
      }),
    );
  }

  it('re-seals server secrets and connections; afterwards the old key opens nothing', async () => {
    await seedUnderOldKey();
    const rotating = containerFor({ secretsKey: NEW, secretsKeyOld: OLD });
    const r = await cli(['rotate-secrets-key'], { container: rotating });
    expect(r.code).toBe(0);
    expect(r.out).toEqual([
      'server secrets: 1 re-sealed, 0 already under the new key',
      'connections: 2 re-sealed, 0 already under the new key',
    ]);
    for (const s of ['access-token-1', 'refresh-token-1', 'pat-token-1', SECRET]) {
      expect(r.all).not.toContain(s);
    }

    const values = await sealedValues();
    expect(values).toHaveLength(4);
    const oldBox = new AesGcmSecretBox(OLD);
    const newBox = new AesGcmSecretBox(NEW);
    for (const v of values) expect(() => oldBox.open(v)).toThrow();
    expect(values.map((v) => newBox.open(v)).toSorted()).toEqual(
      [SECRET, 'access-token-1', 'pat-token-1', 'refresh-token-1'].toSorted(),
    );
    const after = containerFor({ secretsKey: NEW });
    expect(await getServerSecret(after.catalog, 'fake')).toBe(SECRET);

    const again = await cli(['rotate-secrets-key'], { container: rotating });
    expect(again.out).toEqual([
      'server secrets: 0 re-sealed, 1 already under the new key',
      'connections: 0 re-sealed, 2 already under the new key',
    ]);
    const audit = await pg.sql`select meta from platform.audit_log where action = 'secrets.rotate'`;
    expect(audit).toHaveLength(2);
  });

  it('refuses to run without SECRETS_KEY_OLD', async () => {
    const r = await cli(['rotate-secrets-key']);
    expect(r.code).toBe(1);
    expect(r.err.join('\n')).toContain('SECRETS_KEY_OLD');
  });

  it('fails (exit 1) and names what neither key opens', async () => {
    await seedUnderOldKey();
    const stranger = new Uint8Array(randomBytes(32));
    const r = await cli(['rotate-secrets-key'], {
      container: containerFor({ secretsKey: NEW, secretsKeyOld: stranger }),
    });
    expect(r.code).toBe(1);
    expect(r.err.join('\n')).toContain('fake');
    expect(r.err.join('\n')).toMatch(/\/gitea/);
  });
});
