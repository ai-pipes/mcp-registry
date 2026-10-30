import type { OAuthClientConfig } from '../../src/modules/catalog/index.ts';
import {
  type ConnectionsDeps,
  type ConnectionStore,
  type OAuthClient,
  type PendingConnects,
  type PersonalAuthKind,
  type PersonalServer,
  type Principal,
  type ServerConfig,
  type ServerDirectory,
  TokenEndpointError,
  type TokenSet,
} from '../../src/modules/connections/application/ports.ts';
import { createSingleFlight } from '../../src/modules/connections/application/single-flight.ts';
import {
  Connection,
  type ConnectionSnapshot,
} from '../../src/modules/connections/domain/connection.ts';
import type { PendingConnect } from '../../src/modules/connections/domain/pending-connect.ts';
import type { SecretBox } from '../../src/shared-kernel/index.ts';

const key = (userId: string, server: string) => `${userId}\u0000${server}`;

/** Stores snapshots, as a database would; `withLock` serialises callers per key. */
class InMemoryConnectionStore implements ConnectionStore {
  readonly rows = new Map<string, ConnectionSnapshot>();
  private readonly locks = new Map<string, Promise<unknown>>();

  get(userId: string, server: string): Promise<Connection | null> {
    const s = this.rows.get(key(userId, server));
    return Promise.resolve(s ? Connection.restore(s) : null);
  }

  listForUser(userId: string): Promise<Connection[]> {
    return Promise.resolve(
      [...this.rows.values()].filter((s) => s.userId === userId).map((s) => Connection.restore(s)),
    );
  }

  withLock<T>(
    userId: string,
    server: string,
    fn: (current: Connection | null, save: (c: Connection) => Promise<void>) => Promise<T>,
  ): Promise<T> {
    const k = key(userId, server);
    const previous = this.locks.get(k) ?? Promise.resolve();
    const run = previous.then(async () => {
      const current = await this.get(userId, server);
      return fn(current, (c) => {
        this.rows.set(key(c.userId, c.server), c.snapshot());
        return Promise.resolve();
      });
    });
    this.locks.set(
      k,
      run.catch(() => undefined),
    );
    return run;
  }

  delete(userId: string, server: string): Promise<Connection | null> {
    const s = this.rows.get(key(userId, server));
    this.rows.delete(key(userId, server));
    return Promise.resolve(s ? Connection.restore(s) : null);
  }

  listKeys(): Promise<{ userId: string; server: string }[]> {
    return Promise.resolve(
      [...this.rows.values()].map((s) => ({ userId: s.userId, server: s.server })),
    );
  }
}

class InMemoryPending implements PendingConnects {
  readonly rows = new Map<string, PendingConnect>();
  insert(p: PendingConnect): Promise<void> {
    this.rows.set(p.state, p);
    return Promise.resolve();
  }
  consume(state: string): Promise<PendingConnect | null> {
    const p = this.rows.get(state) ?? null;
    this.rows.delete(state);
    return Promise.resolve(p);
  }
}

/** Reversible but never plain: every byte is XOR-ed, with a marker in front. */
const fakeSecretBox: SecretBox = {
  seal: (plain) => new Uint8Array([0xee, ...new TextEncoder().encode(plain).map((b) => b ^ 0x5a)]),
  open: (sealed) => {
    if (sealed[0] !== 0xee) throw new Error('not sealed');
    return new TextDecoder().decode(sealed.slice(1).map((b) => b ^ 0x5a));
  },
};

class FakeOAuth implements OAuthClient {
  refreshCalls: string[] = [];
  exchangeCalls: {
    code: string;
    codeVerifier: string;
    redirectUri: string;
    secret: string | null;
  }[] = [];
  revokeCalls: string[] = [];
  /** Answers refresh with 400 invalid_grant: the grant is gone. */
  refreshFails = false;
  /** Thrown by refresh instead of answering, e.g. a timeout or a 503. */
  refreshThrows: Error | null = null;
  revokeFails = false;
  /** Lets a test hold refresh open to make callers overlap. */
  refreshGate: Promise<void> = Promise.resolve();
  next: TokenSet = { accessToken: 'A-new', refreshToken: 'R-new', expiresAt: null };
  exchangeFails = false;

  authorizeUrl(
    cfg: OAuthClientConfig,
    input: { redirectUri: string; state: string; codeChallenge: string },
  ): URL {
    const u = new URL(cfg.authorizeUrl);
    u.searchParams.set('state', input.state);
    u.searchParams.set('code_challenge', input.codeChallenge);
    u.searchParams.set('redirect_uri', input.redirectUri);
    return u;
  }

  exchangeCode(
    _cfg: OAuthClientConfig,
    secret: string | null,
    input: { code: string; codeVerifier: string; redirectUri: string },
  ): Promise<TokenSet> {
    this.exchangeCalls.push({ ...input, secret });
    if (this.exchangeFails) return Promise.reject(new Error('invalid_grant'));
    return Promise.resolve({ accessToken: 'A1', refreshToken: 'R1', expiresAt: null });
  }

  async refresh(_cfg: OAuthClientConfig, _secret: string | null, refreshToken: string) {
    this.refreshCalls.push(refreshToken);
    await this.refreshGate;
    if (this.refreshThrows) throw this.refreshThrows;
    if (this.refreshFails) throw new TokenEndpointError(400, 'invalid_grant');
    return this.next;
  }

  revoke(_cfg: OAuthClientConfig, _secret: string | null, token: string): Promise<void> {
    this.revokeCalls.push(token);
    return this.revokeFails ? Promise.reject(new Error('boom')) : Promise.resolve();
  }
}

type FakeServer = {
  slug: string;
  name: string;
  authKind: PersonalAuthKind | 'none' | 'shared_secret';
  /** Who may use it; empty = everyone. */
  groups: string[];
  url: string;
  oauth: OAuthClientConfig | null;
  clientSecret: string | null;
  disabled?: boolean;
};

/** Active servers by slug, as the catalog would hand them over. */
class FakeDirectory implements ServerDirectory {
  readonly servers: FakeServer[] = [];

  private static configOf(s: FakeServer): ServerConfig {
    return { slug: s.slug, url: s.url, authKind: s.authKind, oauth: s.oauth };
  }

  find(slug: string): FakeServer | undefined {
    return this.servers.find((s) => s.slug === slug && !s.disabled);
  }

  personalServersFor(principal: Principal): Promise<PersonalServer[]> {
    return Promise.resolve(
      this.servers
        .filter((s) => !s.disabled)
        .filter((s) => s.groups.length === 0 || s.groups.some((g) => principal.groups.includes(g)))
        .flatMap((s): PersonalServer[] =>
          s.authKind === 'oauth_user' || s.authKind === 'pat_user'
            ? [
                {
                  slug: s.slug,
                  name: s.name,
                  authKind: s.authKind,
                  config: FakeDirectory.configOf(s),
                },
              ]
            : [],
        ),
    );
  }

  config(slug: string): Promise<ServerConfig | null> {
    const s = this.find(slug);
    return Promise.resolve(s ? FakeDirectory.configOf(s) : null);
  }

  clientSecret(slug: string): Promise<string | null> {
    return Promise.resolve(this.find(slug)?.clientSecret ?? null);
  }
}

export function makeConnectionsDeps(now: { value: Date }) {
  const store = new InMemoryConnectionStore();
  const pending = new InMemoryPending();
  const oauth = new FakeOAuth();
  const directory = new FakeDirectory();
  const audit: { actorId: string | null; action: string; target?: string }[] = [];
  let n = 0;
  const deps: ConnectionsDeps = {
    connections: store,
    pending,
    servers: directory,
    oauth,
    pkce: { randomToken: () => `rnd-${++n}`, challenge: (v) => `S256(${v})` },
    secretBox: fakeSecretBox,
    flights: createSingleFlight(),
    clock: { now: () => now.value },
    audit: {
      record: (e) => {
        audit.push(e);
        return Promise.resolve();
      },
    },
    redirectUri: 'https://mcp.example.com/api/connections/callback',
  };
  return { deps, store, pending, oauth, directory, audit };
}
