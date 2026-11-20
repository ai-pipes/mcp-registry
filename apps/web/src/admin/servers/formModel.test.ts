import { describe, expect, it } from 'vite-plus/test';
import { build, initialState } from './formModel.ts';

describe('build', () => {
  it('sends the OAuth endpoints and client ID trimmed, like the URL', () => {
    const f = {
      ...initialState(),
      slug: 'hub',
      name: 'Hub',
      url: 'https://hub.example/mcp',
      kind: 'oauth_user' as const,
      authorizeUrl: ' https://idp.example/authorize ',
      tokenUrl: ' https://idp.example/token ',
      revokeUrl: ' https://idp.example/revoke ',
      clientId: ' cid ',
      scopes: 'read',
    };
    const built = build(f, 'create');
    expect(built.ok).toBe(true);
    if (!built.ok) return;
    expect(built.body['auth']).toMatchObject({
      oauth: {
        authorizeUrl: 'https://idp.example/authorize',
        tokenUrl: 'https://idp.example/token',
        revokeUrl: 'https://idp.example/revoke',
        clientId: 'cid',
      },
    });
  });
});
