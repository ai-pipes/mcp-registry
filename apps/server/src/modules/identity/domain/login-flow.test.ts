import { describe, expect, it } from 'vite-plus/test';
import { isLoginStateExpired, loginStateExpiry, normalizeReturnTo } from './login-flow.ts';

describe('normalizeReturnTo', () => {
  it.each([
    ['/servers/gitea', '/servers/gitea'],
    ['/tokens?x=1#h', '/tokens?x=1#h'],
    ['https://evil.example/', '/'],
    ['//evil', '/'],
    ['/\\evil', '/'],
    ['javascript:alert(1)', '/'],
    ['', '/'],
    ['servers', '/'],
    ['/a\r\nb', '/'],
  ])('%j → %j', (input, expected) => {
    expect(normalizeReturnTo(input)).toBe(expected);
  });
});

describe('login state lifetime', () => {
  const now = new Date('2026-01-01T00:00:00Z');
  it('lives for 10 minutes', () => {
    const exp = loginStateExpiry(now);
    expect(exp.getTime() - now.getTime()).toBe(10 * 60_000);
    expect(isLoginStateExpired(exp, new Date(exp.getTime() - 1))).toBe(false);
    expect(isLoginStateExpired(exp, exp)).toBe(true);
  });
});
