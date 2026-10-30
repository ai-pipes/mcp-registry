import { describe, expect, it } from 'vite-plus/test';
import { looksLikeToken, shouldTouchLastUsed, tokenPrefix } from './token.ts';

describe('token format', () => {
  it('accepts sk_ tokens of sane length only', () => {
    expect(looksLikeToken('sk_abc')).toBe(true);
    expect(looksLikeToken('abc')).toBe(false);
    expect(looksLikeToken(`sk_${'a'.repeat(200)}`)).toBe(false);
  });
  it('shows the first 10 characters as the prefix', () => {
    expect(tokenPrefix('sk_0123456789abcdef')).toBe('sk_0123456');
  });
});

describe('shouldTouchLastUsed', () => {
  const now = new Date('2026-01-01T00:10:00Z');
  it('touches when never used or last use is a minute old', () => {
    expect(shouldTouchLastUsed(null, now)).toBe(true);
    expect(shouldTouchLastUsed(new Date(now.getTime() - 60_000), now)).toBe(true);
  });
  it('skips fresh touches', () => {
    expect(shouldTouchLastUsed(new Date(now.getTime() - 59_999), now)).toBe(false);
  });
});
