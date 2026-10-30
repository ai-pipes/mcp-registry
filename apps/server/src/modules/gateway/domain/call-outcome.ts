const OUTCOMES = [
  'ok',
  'upstream_error',
  'timeout',
  'transport_error',
  'denied:not_found',
  'denied:invalid_arguments',
  'denied:quota',
  'denied:needs_connection',
  'denied:expired',
] as const;

/** How a `call_tool` ended, as the call log stores it. */
export type CallOutcome = (typeof OUTCOMES)[number];

/** Why upstream could not be used (the kinds of the shared-kernel UpstreamError). */
type UpstreamFailure = 'timeout' | 'too_large' | 'transport' | 'rejected';

/** Why the person's upstream credential could not be had. */
type CredentialFailure = 'needs_connection' | 'expired' | 'unavailable';

export const CallOutcome = {
  all: OUTCOMES,

  is(value: string): value is CallOutcome {
    return (OUTCOMES as readonly string[]).includes(value);
  },

  /** An answer from upstream: its own `isError` is an upstream error, passed on as is. */
  ofResult(result: { isError?: boolean }): CallOutcome {
    return result.isError === true ? 'upstream_error' : 'ok';
  },

  /**
   * Too large an answer is a transport failure; a timeout has its own outcome. A JSON-RPC error
   * answer (`rejected`) is upstream answering with an error, as its own `isError` would be.
   */
  ofUpstreamFailure(kind: UpstreamFailure): CallOutcome {
    if (kind === 'rejected') return 'upstream_error';
    return kind === 'timeout' ? 'timeout' : 'transport_error';
  },

  /** The OAuth provider being down (`unavailable`) is the service being unreachable, not a denial. */
  ofCredentialFailure(failure: CredentialFailure): CallOutcome {
    if (failure === 'unavailable') return 'transport_error';
    return failure === 'needs_connection' ? 'denied:needs_connection' : 'denied:expired';
  },
};

/** What the gateway does when upstream answers 401 to its credential. */
export type UnauthorizedAction = 'refresh_and_retry' | 'expire' | 'reject';

/**
 * Upstream 401: a personal OAuth token is refreshed once and the call retried; a PAT cannot be
 * renewed, so the connection expires; the server's own key (or no auth) is the admin's to fix.
 */
export function onUpstreamUnauthorized(
  authKind: 'none' | 'shared_secret' | 'oauth_user' | 'pat_user',
): UnauthorizedAction {
  if (authKind === 'oauth_user') return 'refresh_and_retry';
  if (authKind === 'pat_user') return 'expire';
  return 'reject';
}
