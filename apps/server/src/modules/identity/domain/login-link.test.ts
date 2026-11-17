import { describe, expect, it } from 'vite-plus/test';
import {
  isLoginLinkExpired,
  loginLinkExpiry,
  loginLinkUrl,
  looksLikeLoginCode,
  needsBootstrapLink,
  newLoginCode,
} from './login-link.ts';

const t0 = new Date('2026-01-01T00:00:00Z');

describe('login link', () => {
  it('expires 15 minutes after issue', () => {
    expect(loginLinkExpiry(t0)).toEqual(new Date('2026-01-01T00:15:00Z'));
    expect(isLoginLinkExpired(loginLinkExpiry(t0), new Date('2026-01-01T00:14:59.999Z'))).toBe(
      false,
    );
    expect(isLoginLinkExpired(loginLinkExpiry(t0), new Date('2026-01-01T00:15:00Z'))).toBe(true);
  });

  it('marks codes with ll_ and rejects other shapes before the database', () => {
    expect(newLoginCode('abc')).toBe('ll_abc');
    expect(looksLikeLoginCode('ll_abc')).toBe(true);
    expect(looksLikeLoginCode('sk_abc')).toBe(false);
    expect(looksLikeLoginCode('')).toBe(false);
    expect(looksLikeLoginCode('ll_' + 'a'.repeat(100))).toBe(false); // longer than 100 in total
  });

  it('puts the code in the fragment and tolerates a trailing slash', () => {
    expect(loginLinkUrl('https://mcp.example.com', 'll_x')).toBe(
      'https://mcp.example.com/login#link=ll_x',
    );
    expect(loginLinkUrl('https://mcp.example.com/', 'll_x')).toBe(
      'https://mcp.example.com/login#link=ll_x',
    );
  });
});

const admin = (
  over: Partial<{ sub: string; disabled: boolean; lastLoginAt: Date | null }> = {},
) => ({
  sub: 'local-admin',
  disabled: false,
  createdAt: t0,
  lastLoginAt: t0,
  ...over,
});

describe('needsBootstrapLink', () => {
  it('creates the admin when there are no people', () => {
    expect(needsBootstrapLink([], 'local-admin')).toBe('create');
  });
  it('reissues while the only person is the local admin who never signed in', () => {
    expect(needsBootstrapLink([admin()], 'local-admin')).toBe('reissue');
    expect(needsBootstrapLink([admin({ lastLoginAt: null })], 'local-admin')).toBe('reissue');
  });
  it('does nothing after a real sign-in', () => {
    expect(
      needsBootstrapLink([admin({ lastLoginAt: new Date(t0.getTime() + 1) })], 'local-admin'),
    ).toBe('none');
  });
  it('does nothing for a disabled admin, another person or several people', () => {
    expect(needsBootstrapLink([admin({ disabled: true })], 'local-admin')).toBe('none');
    expect(needsBootstrapLink([admin({ sub: 'ann' })], 'local-admin')).toBe('none');
    expect(needsBootstrapLink([admin(), admin({ sub: 'ann' })], 'local-admin')).toBe('none');
  });
});
