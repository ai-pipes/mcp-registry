import type {
  IdentityDeps,
  OidcProvider,
  PendingLogin,
  SessionRecord,
  TokenRecord,
} from '../../src/modules/identity/application/ports.ts';
import {
  IdentityError,
  type OidcClaims,
  type User,
} from '../../src/modules/identity/domain/user.ts';

class FakeClock {
  constructor(private t = new Date('2026-01-01T00:00:00Z')) {}
  now(): Date {
    return new Date(this.t);
  }
  advance(ms: number): void {
    this.t = new Date(this.t.getTime() + ms);
  }
}

/** `next` is what the provider answers; `null` makes the exchange fail. */
class FakeOidc implements OidcProvider {
  next: OidcClaims | null = null;
  lastExchange: { code: string; iss?: string } | null = null;
  /** Makes both methods fail as an unreachable provider does. */
  unavailable = false;
  authorizationUrl(input: { state: string; codeChallenge: string; nonce: string }): Promise<URL> {
    if (this.unavailable) {
      return Promise.reject(new IdentityError('idp_unavailable', 'provider unreachable'));
    }
    const url = new URL('https://id.example/authorize');
    url.searchParams.set('state', input.state);
    url.searchParams.set('nonce', input.nonce);
    url.searchParams.set('code_challenge', input.codeChallenge);
    return Promise.resolve(url);
  }
  exchange(input: { code: string; iss?: string }): Promise<OidcClaims> {
    this.lastExchange = input;
    if (this.unavailable) {
      return Promise.reject(new IdentityError('idp_unavailable', 'provider unreachable'));
    }
    return this.next ? Promise.resolve(this.next) : Promise.reject(new Error('exchange failed'));
  }
}

export function makeDeps() {
  const users = new Map<string, User>();
  const tokens = new Map<string, TokenRecord>();
  const sessions = new Map<string, SessionRecord>();
  const pendingLogins = new Map<string, PendingLogin>();
  const auditEntries: { actorId: string | null; action: string; target?: string }[] = [];
  const clock = new FakeClock();
  const oidc = new FakeOidc();
  let counter = 0;
  let nextId = 0;

  const deps: IdentityDeps = {
    users: {
      findById: (id) => Promise.resolve(users.get(id) ?? null),
      findBySub: (sub) =>
        Promise.resolve([...users.values()].find((u) => u.oidcSub === sub) ?? null),
      upsertFromLogin: ({ claims, role, at }) => {
        const existing = [...users.values()].find((u) => u.oidcSub === claims.sub);
        const user: User = {
          id: existing?.id ?? `user-${++nextId}`,
          oidcSub: claims.sub,
          name: claims.name,
          email: claims.email,
          groups: claims.groups,
          role,
          disabled: existing?.disabled ?? false,
          createdAt: existing?.createdAt ?? at,
          lastLoginAt: at,
        };
        users.set(user.id, user);
        return Promise.resolve(user);
      },
      list: () => Promise.resolve([...users.values()]),
      setDisabled: (id, disabled) => {
        const u = users.get(id);
        if (!u) return Promise.resolve(false);
        u.disabled = disabled;
        return Promise.resolve(true);
      },
    },
    tokens: {
      insert: (t) => {
        const rec: TokenRecord = { ...t, id: `tok-${++nextId}`, lastUsedAt: null, revokedAt: null };
        tokens.set(rec.id, rec);
        return Promise.resolve(rec);
      },
      findByHash: (hash) =>
        Promise.resolve([...tokens.values()].find((t) => t.hash === hash) ?? null),
      findById: (id) => Promise.resolve(tokens.get(id) ?? null),
      listByUser: (userId) =>
        Promise.resolve([...tokens.values()].filter((t) => t.userId === userId)),
      revoke: (id, at) => {
        const t = tokens.get(id);
        if (t && !t.revokedAt) t.revokedAt = at;
        return Promise.resolve();
      },
      revokeAllByUser: (userId, at) => {
        for (const t of tokens.values()) if (t.userId === userId && !t.revokedAt) t.revokedAt = at;
        return Promise.resolve();
      },
      touch: (id, at) => {
        const t = tokens.get(id);
        if (t) t.lastUsedAt = at;
        return Promise.resolve();
      },
    },
    sessions: {
      insert: (s) => {
        sessions.set(s.idHash, s);
        return Promise.resolve();
      },
      findByHash: (h) => Promise.resolve(sessions.get(h) ?? null),
      touch: (h, at) => {
        const s = sessions.get(h);
        if (s) s.lastSeenAt = at;
        return Promise.resolve();
      },
      delete: (h) => {
        sessions.delete(h);
        return Promise.resolve();
      },
      deleteByUser: (userId) => {
        for (const [h, s] of sessions) if (s.userId === userId) sessions.delete(h);
        return Promise.resolve();
      },
      deleteDead: ({ expiresAtOrBefore, lastSeenBefore }) => {
        let n = 0;
        for (const [h, s] of sessions) {
          if (s.expiresAt <= expiresAtOrBefore || s.lastSeenAt < lastSeenBefore) {
            sessions.delete(h);
            n += 1;
          }
        }
        return Promise.resolve(n);
      },
    },
    pendingLogins: {
      insert: (p) => {
        pendingLogins.set(p.state, p);
        return Promise.resolve();
      },
      consume: (state) => {
        const p = pendingLogins.get(state) ?? null;
        pendingLogins.delete(state);
        return Promise.resolve(p);
      },
    },
    oidc,
    policy: { loginGroup: 'mcp-users', adminGroup: 'mcp-admins' },
    sha256: {
      hex: (s) => `h:${s}`,
      base64url: (s) => `b:${s}`,
      timingSafeEqual: (a, b) => a === b,
    },
    tokenLimiter: {
      hit: () => ({ allowed: true, retryAfterSec: 0 }),
      check: () => ({ allowed: true, retryAfterSec: 0 }),
      reset: () => undefined,
    },
    audit: {
      record: (e) => {
        auditEntries.push(e);
        return Promise.resolve();
      },
    },
    clock,
    random: {
      base64url: (n) => `r${++counter}-${'x'.repeat(n)}`,
    },
  };
  return { deps, users, tokens, sessions, pendingLogins, auditEntries, clock, oidc };
}

/** The provider URL of a started login; throws when the start failed. */
export function startedUrl(r: { ok: true; value: URL } | { ok: false; error: Error }): URL {
  if (!r.ok) throw r.error;
  return r.value;
}
