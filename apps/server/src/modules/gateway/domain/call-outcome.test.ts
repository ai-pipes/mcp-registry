import { describe, expect, it } from 'vite-plus/test';
import { CallOutcome, onUpstreamUnauthorized } from './call-outcome.ts';

describe('CallOutcome', () => {
  it('lists every outcome of the call log', () => {
    expect(CallOutcome.all).toEqual([
      'ok',
      'upstream_error',
      'timeout',
      'transport_error',
      'denied:not_found',
      'denied:invalid_arguments',
      'denied:quota',
      'denied:needs_connection',
      'denied:expired',
    ]);
    expect(CallOutcome.is('denied:quota')).toBe(true);
    expect(CallOutcome.is('denied:other')).toBe(false);
  });

  it('an upstream result is ok unless upstream flagged it isError', () => {
    expect(CallOutcome.ofResult({})).toBe('ok');
    expect(CallOutcome.ofResult({ isError: false })).toBe('ok');
    expect(CallOutcome.ofResult({ isError: true })).toBe('upstream_error');
  });

  it('a timeout is logged as timeout; too large and transport failures as transport_error', () => {
    expect(CallOutcome.ofUpstreamFailure('timeout')).toBe('timeout');
    expect(CallOutcome.ofUpstreamFailure('too_large')).toBe('transport_error');
    expect(CallOutcome.ofUpstreamFailure('transport')).toBe('transport_error');
  });

  it('a JSON-RPC error answer is upstream answering, not a transport failure', () => {
    expect(CallOutcome.ofUpstreamFailure('rejected')).toBe('upstream_error');
  });

  it('a credential failure: missing → needs_connection, expired → expired, provider down → transport', () => {
    expect(CallOutcome.ofCredentialFailure('needs_connection')).toBe('denied:needs_connection');
    expect(CallOutcome.ofCredentialFailure('expired')).toBe('denied:expired');
    expect(CallOutcome.ofCredentialFailure('unavailable')).toBe('transport_error');
  });
});

describe('onUpstreamUnauthorized', () => {
  it('OAuth: refresh once and retry; PAT: expire; server key or no auth: rejected', () => {
    expect(onUpstreamUnauthorized('oauth_user')).toBe('refresh_and_retry');
    expect(onUpstreamUnauthorized('pat_user')).toBe('expire');
    expect(onUpstreamUnauthorized('shared_secret')).toBe('reject');
    expect(onUpstreamUnauthorized('none')).toBe('reject');
  });
});
