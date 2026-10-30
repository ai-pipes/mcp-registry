import { describe, expect, it } from 'vite-plus/test';
import { type SyncFailure, syncFailureText, SyncOwner, SyncRecord } from './sync.ts';

const SYNC_FAILURES: SyncFailure[] = [
  'needs_connection',
  'no_secret',
  'unauthorized',
  'timeout',
  'too_large',
  'transport',
  'credentials_unavailable',
  'rejected',
  'internal',
];

describe('syncFailureText', () => {
  it('gives every failure a distinct, non-empty English text', () => {
    const texts = SYNC_FAILURES.map(syncFailureText);
    expect(new Set(texts).size).toBe(SYNC_FAILURES.length);
    for (const t of texts) expect(t).toMatch(/[A-Za-z]/);
  });

  it('an unreachable OAuth provider is not called an upstream failure', () => {
    expect(syncFailureText('credentials_unavailable')).toBe('The OAuth provider is unavailable');
  });

  it('a JSON-RPC error answer to tools/list has its own text', () => {
    expect(syncFailureText('rejected')).toBe('Upstream answered tools/list with a JSON-RPC error');
  });

  it('needs_connection reads as specified', () => {
    expect(syncFailureText('needs_connection')).toBe(
      'An administrator needs to connect this server',
    );
  });
});

describe('SyncRecord', () => {
  const t0 = new Date('2026-10-30T10:00:00Z');
  it('a success names who ran it; a failure names nobody and carries the text', () => {
    expect(SyncRecord.success('admin-1', t0)).toEqual({
      startedAt: t0,
      error: null,
      syncedBy: 'admin-1',
    });
    expect(SyncRecord.failure('no_secret', t0)).toEqual({
      startedAt: t0,
      error: 'The server secret is not set',
      syncedBy: null,
    });
  });
});

describe('SyncOwner', () => {
  it('lends their connection only while they are an enabled admin', () => {
    expect(SyncOwner.mayLend('admin-1', true)).toBe(true);
    // Demoted or disabled since: their connection is not used any more.
    expect(SyncOwner.mayLend('admin-1', false)).toBe(false);
    // The scheduler on a server nobody synchronised.
    expect(SyncOwner.mayLend(null, true)).toBe(false);
  });

  it('a missing or expired connection needs one; an unreachable provider is not the owner’s fault', () => {
    expect(SyncOwner.failureOf('needs_connection')).toBe('needs_connection');
    expect(SyncOwner.failureOf('expired')).toBe('needs_connection');
    expect(SyncOwner.failureOf('unavailable')).toBe('credentials_unavailable');
  });
});
