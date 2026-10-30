import { describe, expect, it } from 'vite-plus/test';
import { isProviderOutage } from './openid-provider.ts';

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
