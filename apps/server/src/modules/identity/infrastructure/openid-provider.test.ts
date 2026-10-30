import { describe, expect, it } from 'vite-plus/test';
import { groupsOf, isProviderOutage } from './openid-provider.ts';

const withCode = (e: Error, code: string) => Object.assign(e, { code });

describe('isProviderOutage', () => {
  it('no answer: a network TypeError, a timeout or an abort', () => {
    expect(isProviderOutage(new TypeError('fetch failed'))).toBe(true);
    expect(isProviderOutage(new DOMException('t', 'TimeoutError'))).toBe(true);
    expect(isProviderOutage(new DOMException('a', 'AbortError'))).toBe(true);
  });

  it('a 5xx answer, on the error or on its cause (the Response)', () => {
    expect(isProviderOutage(Object.assign(new Error('x'), { status: 503 }))).toBe(true);
    expect(isProviderOutage(new Error('x', { cause: { status: 502 } }))).toBe(true);
  });

  it('not an outage: OAuth errors, 4xx, argument errors, non-errors', () => {
    expect(isProviderOutage(Object.assign(new Error('x'), { status: 400 }))).toBe(false);
    expect(isProviderOutage(new Error('x', { cause: { status: 401 } }))).toBe(false);
    expect(isProviderOutage(withCode(new TypeError('bad arg'), 'ERR_INVALID_ARG_TYPE'))).toBe(
      false,
    );
    expect(isProviderOutage(new Error('id token nonce mismatch'))).toBe(false);
    expect(isProviderOutage('boom')).toBe(false);
  });
});

describe('groupsOf', () => {
  it('a list of strings, as most providers emit it', () => {
    expect(groupsOf(['mcp-users', 'mcp-admins'])).toEqual(['mcp-users', 'mcp-admins']);
    expect(groupsOf([])).toEqual([]);
  });

  it('the keys of an object, as Zitadel emits project roles', () => {
    expect(groupsOf({ 'mcp-users': { '123': 'org.example.com' }, 'mcp-admins': {} })).toEqual([
      'mcp-users',
      'mcp-admins',
    ]);
  });

  it('nothing usable: absent, a string, a mixed list, null', () => {
    expect(groupsOf(undefined)).toBeNull();
    expect(groupsOf('mcp-users')).toBeNull();
    expect(groupsOf(['mcp-users', 1])).toBeNull();
    expect(groupsOf(null)).toBeNull();
  });
});
