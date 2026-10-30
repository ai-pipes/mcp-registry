import { describe, expect, it } from 'vite-plus/test';
import { Connection, isRefreshRejected, PatToken } from './connection.ts';
import {
  canComplete,
  isPendingConnectExpired,
  pendingConnectExpiry,
  startedWithConfig,
} from './pending-connect.ts';
import { serverFingerprint } from './server-fingerprint.ts';

const now = new Date('2026-10-30T12:00:00.000Z');
const at = (ms: number) => new Date(now.getTime() + ms);
const bytes = (s: string) => new TextEncoder().encode(s);
const OAUTH_CFG = {
  authorizeUrl: 'https://git.example.com/a',
  tokenUrl: 'https://git.example.com/t',
  clientId: 'cid',
};
const FP = serverFingerprint({
  url: 'https://mcp.example.com/gitea',
  authKind: 'oauth_user',
  oauth: OAUTH_CFG,
});

function oauth(over: { expiresAt?: Date | null; refresh?: Uint8Array | null } = {}) {
  return Connection.connectOAuth({
    userId: 'u1',
    server: 'gitea',
    access: bytes('A1'),
    refresh: over.refresh === undefined ? bytes('R1') : over.refresh,
    expiresAt: over.expiresAt === undefined ? at(3600_000) : over.expiresAt,
    issuedFor: FP,
    revokeUrl: null,
    now,
  });
}

describe('Connection.credentialPlan', () => {
  it('uses a fresh OAuth token as is', () => {
    expect(oauth().credentialPlan(now, { forceRefresh: false, config: FP }).action).toBe('use');
    expect(
      oauth({ expiresAt: at(0) }).credentialPlan(now, { forceRefresh: false, config: FP }),
    ).toEqual({
      action: 'refresh',
      refreshToken: bytes('R1'),
    });
  });

  it('refreshes ahead when less than 60 s are left, or already expired', () => {
    expect(
      oauth({ expiresAt: at(30_000) }).credentialPlan(now, { forceRefresh: false, config: FP })
        .action,
    ).toBe('refresh');
    expect(
      oauth({ expiresAt: at(59_999) }).credentialPlan(now, { forceRefresh: false, config: FP })
        .action,
    ).toBe('refresh');
    expect(
      oauth({ expiresAt: at(60_000) }).credentialPlan(now, { forceRefresh: false, config: FP })
        .action,
    ).toBe('use');
    expect(
      oauth({ expiresAt: at(-1) }).credentialPlan(now, { forceRefresh: false, config: FP }).action,
    ).toBe('refresh');
  });

  it('a token without an expiry is used until forced', () => {
    const c = oauth({ expiresAt: null });
    expect(c.credentialPlan(now, { forceRefresh: false, config: FP }).action).toBe('use');
    expect(c.credentialPlan(now, { forceRefresh: true, config: FP }).action).toBe('refresh');
  });

  it('without a refresh token: used while valid, expired once it is not or was rejected', () => {
    expect(
      oauth({ expiresAt: at(30_000), refresh: null }).credentialPlan(now, {
        forceRefresh: false,
        config: FP,
      }).action,
    ).toBe('use');
    expect(
      oauth({ expiresAt: at(0), refresh: null }).credentialPlan(now, {
        forceRefresh: false,
        config: FP,
      }).action,
    ).toBe('expire');
    expect(
      oauth({ refresh: null }).credentialPlan(now, { forceRefresh: true, config: FP }).action,
    ).toBe('expire');
  });

  it('an expired connection is never refreshed again', () => {
    const c = oauth({ expiresAt: at(-1) });
    c.expire(now);
    expect(c.status).toBe('expired');
    expect(c.credentialPlan(now, { forceRefresh: true, config: FP }).action).toBe('expired');
  });

  it('a PAT has no expiry and cannot be refreshed', () => {
    const c = Connection.connectPat({
      userId: 'u1',
      server: 'yt',
      token: bytes('P'),
      issuedFor: FP,
      now,
    });
    expect(c.kind).toBe('pat');
    expect(c.expiresAt).toBeNull();
    expect(c.refreshToken).toBeNull();
    expect(c.credentialPlan(now, { forceRefresh: false, config: FP }).action).toBe('use');
    expect(c.credentialPlan(now, { forceRefresh: true, config: FP }).action).toBe('use');
    c.expire(now);
    expect(c.credentialPlan(now, { forceRefresh: false, config: FP }).action).toBe('expired');
  });
});

describe('the server config the connection was issued for', () => {
  it('fingerprints url, auth kind, OAuth endpoints (revoke too) and client id; not scopes', () => {
    const base = { url: 'https://mcp.example.com/gitea', authKind: 'oauth_user' as const };
    expect(serverFingerprint({ ...base, oauth: OAUTH_CFG })).toBe(FP);
    const others = [
      serverFingerprint({ ...base, url: 'https://evil.example.com/mcp', oauth: OAUTH_CFG }),
      serverFingerprint({ ...base, authKind: 'pat_user', oauth: null }),
      serverFingerprint({ ...base, oauth: { ...OAUTH_CFG, authorizeUrl: 'https://x/a' } }),
      serverFingerprint({ ...base, oauth: { ...OAUTH_CFG, tokenUrl: 'https://evil/t' } }),
      serverFingerprint({ ...base, oauth: { ...OAUTH_CFG, clientId: 'other' } }),
    ];
    expect(new Set([FP, ...others]).size).toBe(6);
    const withScopes = { ...OAUTH_CFG, scopes: ['x'] };
    expect(serverFingerprint({ ...base, oauth: withScopes })).toBe(FP);
    // The revoke URL is bound to the connection itself (only disconnect uses it): turning
    // revocation on or changing its URL does not expire anyone's connection.
    expect(serverFingerprint({ ...base, oauth: { ...OAUTH_CFG, revokeUrl: 'https://r' } })).toBe(
      FP,
    );
  });

  it('a changed server config expires the connection instead of handing out its token', () => {
    const changed = serverFingerprint({
      url: 'https://evil.example.com/mcp',
      authKind: 'oauth_user',
      oauth: OAUTH_CFG,
    });
    const c = oauth();
    expect(c.credentialPlan(now, { forceRefresh: false, config: changed })).toEqual({
      action: 'expire',
      reason: 'config_changed',
    });
    expect(c.issuedFor(FP)).toBe(true);
    expect(c.issuedFor(changed)).toBe(false);
    expect(c.statusFor(FP)).toBe('ready');
    expect(c.statusFor(changed)).toBe('expired');
    const pat = Connection.connectPat({
      userId: 'u1',
      server: 'yt',
      token: bytes('P'),
      issuedFor: FP,
      now,
    });
    expect(pat.credentialPlan(now, { forceRefresh: false, config: changed }).action).toBe('expire');
  });

  it('an expired token without a refresh token expires for that reason', () => {
    expect(
      oauth({ expiresAt: at(0), refresh: null }).credentialPlan(now, {
        forceRefresh: false,
        config: FP,
      }),
    ).toEqual({ action: 'expire', reason: 'token_expired' });
  });
});

function issued(revokeUrl: string | null) {
  return Connection.connectOAuth({
    userId: 'u1',
    server: 'gitea',
    access: bytes('A1'),
    refresh: bytes('R1'),
    expiresAt: null,
    issuedFor: FP,
    revokeUrl,
    now,
  });
}

describe('Connection.mayRevokeAt', () => {
  it('only the revoke URL in place when the tokens were issued', () => {
    const c = issued('https://git.example.com/revoke');
    expect(c.revokeUrl).toBe('https://git.example.com/revoke');
    expect(c.mayRevokeAt('https://git.example.com/revoke')).toBe(true);
    expect(c.mayRevokeAt('https://evil.example.com/revoke')).toBe(false);
    expect(c.mayRevokeAt(undefined)).toBe(false);
  });

  it('no revoke URL at issue time (added later) or a PAT: never revoked', () => {
    expect(issued(null).mayRevokeAt('https://git.example.com/revoke')).toBe(false);
    const pat = Connection.connectPat({
      userId: 'u1',
      server: 'yt',
      token: bytes('P'),
      issuedFor: FP,
      now,
    });
    expect(pat.revokeUrl).toBeNull();
    expect(pat.mayRevokeAt('https://git.example.com/revoke')).toBe(false);
  });

  it('a refresh keeps the issuing revoke URL', () => {
    const c = issued('https://git.example.com/revoke');
    c.refreshed({ access: bytes('A2'), refresh: null, expiresAt: null }, now);
    expect(c.mayRevokeAt('https://git.example.com/revoke')).toBe(true);
  });
});

describe('isRefreshRejected', () => {
  it('only an OAuth error answer of the token endpoint means the grant is gone', () => {
    expect(isRefreshRejected({ status: 400, error: 'invalid_grant' })).toBe(true);
    expect(isRefreshRejected({ status: 400, error: null })).toBe(true);
    expect(isRefreshRejected({ status: 500, error: 'invalid_grant' })).toBe(true);
    expect(isRefreshRejected({ status: 503, error: null })).toBe(false);
    expect(isRefreshRejected({ status: 500, error: 'server_error' })).toBe(false);
    expect(isRefreshRejected({ status: 429, error: null })).toBe(false);
    // No answer at all: timeout, DNS, connection refused.
    expect(isRefreshRejected(null)).toBe(false);
  });

  it("the gateway's own client credentials failing is not a refusal of the person's grant", () => {
    // YouTrack Hub: a wrong client secret is HTTP 401 with invalid_client.
    expect(isRefreshRejected({ status: 401, error: 'invalid_client' })).toBe(false);
    // Gitea: a wrong client secret is HTTP 400 with invalid_client.
    expect(isRefreshRejected({ status: 400, error: 'invalid_client' })).toBe(false);
    // unauthorized_client with 401 is client authentication failing too.
    expect(isRefreshRejected({ status: 401, error: 'unauthorized_client' })).toBe(false);
  });

  it("a refused grant still expires, in both providers' shapes", () => {
    // Hub: a revoked or used refresh token.
    expect(isRefreshRejected({ status: 400, error: 'invalid_grant' })).toBe(true);
    // Gitea answers an unparseable refresh token with unauthorized_client and HTTP 400.
    expect(isRefreshRejected({ status: 400, error: 'unauthorized_client' })).toBe(true);
    expect(isRefreshRejected({ status: 401, error: null })).toBe(true);
  });
});

describe('Connection.usableWhileProviderDown', () => {
  it('the current token while it has not expired, unless upstream already rejected it', () => {
    const c = oauth({ expiresAt: at(30_000) });
    expect(c.usableWhileProviderDown(now, { forceRefresh: false })).toBe(true);
    expect(c.usableWhileProviderDown(at(29_999), { forceRefresh: false })).toBe(true);
    expect(c.usableWhileProviderDown(at(30_000), { forceRefresh: false })).toBe(false);
    expect(c.usableWhileProviderDown(now, { forceRefresh: true })).toBe(false);
    expect(oauth({ expiresAt: null }).usableWhileProviderDown(now, { forceRefresh: false })).toBe(
      true,
    );
    c.expire(now);
    expect(c.usableWhileProviderDown(now, { forceRefresh: false })).toBe(false);
  });
});

describe('Connection.refreshed', () => {
  it('takes the new tokens and keeps the old refresh token when none came back', () => {
    const c = oauth({ expiresAt: at(10_000) });
    const later = at(5_000);
    c.refreshed({ access: bytes('A2'), refresh: null, expiresAt: at(3_605_000) }, later);
    expect(c.accessToken).toEqual(bytes('A2'));
    expect(c.refreshToken).toEqual(bytes('R1'));
    expect(c.expiresAt).toEqual(at(3_605_000));
    expect(c.updatedAt).toEqual(later);
    expect(c.status).toBe('ready');
    c.refreshed({ access: bytes('A3'), refresh: bytes('R2'), expiresAt: null }, later);
    expect(c.refreshToken).toEqual(bytes('R2'));
  });
});

describe('Connection.isNewerThan', () => {
  it('tells whether someone else changed the connection since it was read', () => {
    const seen = oauth();
    const same = Connection.restore({ ...seen.snapshot() });
    expect(same.isNewerThan(seen)).toBe(false);
    same.refreshed({ access: bytes('A2'), refresh: null, expiresAt: null }, at(1));
    expect(same.isNewerThan(seen)).toBe(true);
  });
});

describe('Connection.replaceSealed', () => {
  it('swaps the sealed bytes only: status, expiry and updatedAt stay', () => {
    const c = oauth();
    c.expire(at(1));
    const before = c.snapshot();
    c.replaceSealed({ access: bytes('A1*'), refresh: bytes('R1*') });
    expect(c.snapshot()).toEqual({ ...before, access: bytes('A1*'), refresh: bytes('R1*') });
  });

  it('refuses to add or drop a refresh token: that would change the credential', () => {
    expect(() => oauth().replaceSealed({ access: bytes('A'), refresh: null })).toThrow();
    expect(() =>
      oauth({ refresh: null }).replaceSealed({ access: bytes('A'), refresh: bytes('R') }),
    ).toThrow();
  });
});

describe('Connection.restore', () => {
  it('round-trips through a snapshot', () => {
    const c = oauth();
    expect(Connection.restore(c.snapshot()).snapshot()).toEqual(c.snapshot());
  });
});

describe('PatToken.parse', () => {
  it('trims and requires a non-empty single-line token', () => {
    expect(PatToken.parse('  abc  ')).toEqual({ ok: true, value: 'abc' });
    for (const bad of ['', '   ', 'a\nb', 'a\rb', 'x'.repeat(4097)]) {
      const r = PatToken.parse(bad);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_pat');
    }
  });
});

describe('pending connect', () => {
  it('lives 10 minutes', () => {
    const exp = pendingConnectExpiry(now);
    expect(exp).toEqual(at(600_000));
    expect(isPendingConnectExpired(exp, at(599_999))).toBe(false);
    expect(isPendingConnectExpired(exp, at(600_000))).toBe(true);
  });

  it('is finished only by the person who started it, before it expires, for the same config', () => {
    const p = {
      state: 's',
      userId: 'u1',
      server: 'gitea',
      codeVerifier: 'v',
      issuedFor: FP,
      expiresAt: at(600_000),
    };
    expect(canComplete(p, { userId: 'u1', now })).toBe(true);
    expect(canComplete(p, { userId: 'u2', now })).toBe(false);
    expect(canComplete(p, { userId: 'u1', now: at(600_000) })).toBe(false);
    expect(startedWithConfig(p, FP)).toBe(true);
    expect(startedWithConfig(p, 'other')).toBe(false);
  });
});
