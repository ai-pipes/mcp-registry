import { describe, expect, it } from 'vite-plus/test';
import {
  isLoginLinkExpired,
  loginLinkExpiry,
  loginLinkUrl,
  looksLikeLoginCode,
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
