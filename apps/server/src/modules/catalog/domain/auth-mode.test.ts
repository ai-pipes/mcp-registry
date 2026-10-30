import { describe, expect, it } from 'vite-plus/test';
import { AuthMode, type OAuthClientConfig } from './auth-mode.ts';

const oauth: OAuthClientConfig = {
  authorizeUrl: 'https://git.example.com/login/oauth/authorize',
  tokenUrl: 'https://git.example.com/login/oauth/access_token',
  clientId: 'abc',
  scopes: ['read:user'],
};

const withScopes = (scopes: string[]): AuthMode => ({
  kind: 'oauth_user',
  oauth: { ...oauth, scopes },
  header: 'Authorization',
  template: 'Bearer {secret}',
});

describe('AuthMode.parse', () => {
  it('accepts none', () => {
    expect(AuthMode.parse({ kind: 'none' })).toEqual({ ok: true, value: { kind: 'none' } });
  });

  it('requires {secret} in the template', () => {
    for (const kind of ['shared_secret', 'pat_user'] as const) {
      const r = AuthMode.parse({ kind, header: 'Authorization', template: 'Bearer' });
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_auth');
    }
    expect(
      AuthMode.parse({ kind: 'oauth_user', oauth, header: 'Authorization', template: 'x' }).ok,
    ).toBe(false);
  });

  it('rejects bad header names and newlines in the template', () => {
    for (const header of ['', 'Bad Header', 'X:Y', 'X\nY']) {
      expect(AuthMode.parse({ kind: 'shared_secret', header, template: '{secret}' }).ok).toBe(
        false,
      );
    }
    expect(
      AuthMode.parse({ kind: 'shared_secret', header: 'X-Key', template: '{secret}\nX: 1' }).ok,
    ).toBe(false);
  });

  it('validates the oauth client config', () => {
    const good: AuthMode & { kind: 'oauth_user' } = {
      kind: 'oauth_user',
      oauth,
      header: 'Authorization',
      template: 'Bearer {secret}',
    };
    expect(AuthMode.parse(good).ok).toBe(true);
    expect(AuthMode.parse({ ...good, oauth: { ...oauth, authorizeUrl: 'nope' } }).ok).toBe(false);
    expect(AuthMode.parse({ ...good, oauth: { ...oauth, tokenUrl: 'ftp://x/y' } }).ok).toBe(false);
    expect(AuthMode.parse({ ...good, oauth: { ...oauth, clientId: ' ' } }).ok).toBe(false);
    expect(AuthMode.parse({ ...good, oauth: { ...oauth, revokeUrl: 'nope' } }).ok).toBe(false);
    expect(
      AuthMode.parse({ ...good, oauth: { ...oauth, revokeUrl: 'https://x.example/revoke' } }).ok,
    ).toBe(true);
  });

  it('accepts extra authorize parameters and rejects ones that would override the flow', () => {
    const good: AuthMode & { kind: 'oauth_user' } = {
      kind: 'oauth_user',
      oauth: { ...oauth, extraAuthorizeParams: { access_type: 'offline' } },
      header: 'Authorization',
      template: 'Bearer {secret}',
    };
    expect(AuthMode.parse(good)).toEqual({ ok: true, value: good });
    for (const key of ['state', 'redirect_uri', 'code_challenge', 'client_id', 'scope', '']) {
      const r = AuthMode.parse({
        ...good,
        oauth: { ...oauth, extraAuthorizeParams: { [key]: 'x' } },
      });
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_auth');
    }
    const bad = { ...good, oauth: { ...oauth, extraAuthorizeParams: { a: 1 } } };
    expect(AuthMode.parse(JSON.parse(JSON.stringify(bad))).ok).toBe(false);
  });

  it('rejects the openid scope: no ID token can be checked against the admin-entered endpoints', () => {
    for (const scopes of [['openid'], ['read', 'openid'], ['OpenID']]) {
      const r = AuthMode.parse(withScopes(scopes));
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error.code).toBe('invalid_auth');
    }
    expect(AuthMode.parse(withScopes(['openid-connect', 'read'])).ok).toBe(true);
  });

  it('rejects an unknown kind', () => {
    expect(AuthMode.parse(JSON.parse('{"kind":"basic"}')).ok).toBe(false);
  });
});

describe('AuthMode.headerFor', () => {
  it('builds the header from the template', () => {
    expect(
      AuthMode.headerFor({ kind: 'shared_secret', header: 'X-API-Key', template: '{secret}' }, 'k'),
    ).toEqual({ 'X-API-Key': 'k' });
    expect(
      AuthMode.headerFor(
        { kind: 'pat_user', header: 'Authorization', template: 'Bearer {secret}' },
        't',
      ),
    ).toEqual({ Authorization: 'Bearer t' });
  });

  it('does not interpret $ patterns in the secret', () => {
    expect(
      AuthMode.headerFor({ kind: 'shared_secret', header: 'H', template: 'k={secret}' }, "a$&b$'"),
    ).toEqual({ H: "k=a$&b$'" });
  });

  it('gives no headers for none', () => {
    expect(AuthMode.headerFor({ kind: 'none' }, 'ignored')).toEqual({});
  });

  it('works for oauth_user', () => {
    expect(
      AuthMode.headerFor(
        { kind: 'oauth_user', oauth, header: 'Authorization', template: 'Bearer {secret}' },
        'tok',
      ),
    ).toEqual({ Authorization: 'Bearer tok' });
  });
});

describe('AuthMode.credentialSource', () => {
  it('none needs nothing, shared_secret the server secret, oauth/pat a person', () => {
    expect(AuthMode.credentialSource({ kind: 'none' })).toBe('none');
    expect(
      AuthMode.credentialSource({ kind: 'shared_secret', header: 'X-Key', template: '{secret}' }),
    ).toBe('server_secret');
    expect(
      AuthMode.credentialSource({
        kind: 'pat_user',
        header: 'Authorization',
        template: 'token {secret}',
      }),
    ).toBe('personal');
    expect(
      AuthMode.credentialSource({
        kind: 'oauth_user',
        header: 'Authorization',
        template: 'Bearer {secret}',
        oauth: {
          authorizeUrl: 'https://a.example',
          tokenUrl: 'https://t.example',
          clientId: 'c',
          scopes: [],
        },
      }),
    ).toBe('personal');
  });
});
