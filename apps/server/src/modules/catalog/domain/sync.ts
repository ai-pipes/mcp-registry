import type { UpstreamErrorKind } from '../../../shared-kernel/index.ts';

/** Why a catalog synchronisation did not happen. */
export type SyncFailure =
  | 'needs_connection'
  /** The admin's OAuth token needed a refresh and the OAuth provider did not answer. */
  | 'credentials_unavailable'
  | 'no_secret'
  /** An unexpected error inside the synchronisation; the details go to the log only. */
  | 'internal'
  | UpstreamErrorKind;

const TEXT: Record<SyncFailure, string> = {
  needs_connection: 'An administrator needs to connect this server',
  credentials_unavailable: 'The OAuth provider is unavailable',
  no_secret: 'The server secret is not set',
  internal: 'Internal sync error',
  unauthorized: 'Upstream rejected the credentials (HTTP 401)',
  timeout: 'Upstream did not respond in time',
  too_large: 'The upstream response exceeds the allowed size',
  transport: 'Could not reach the upstream',
  rejected: 'Upstream answered tools/list with a JSON-RPC error',
};

/** Why a person's own credential to a server could not be had (the connections module's answer). */
export type PersonalCredentialFailure = 'needs_connection' | 'expired' | 'unavailable';

/**
 * The owner of a synchronisation's credentials (`Server.syncCredentialsOwner`): an `oauth_user` /
 * `pat_user` server is synchronised with that person's own connection.
 */
export const SyncOwner = {
  /**
   * Their connection is used only while they are an enabled admin: a demoted or disabled admin's
   * connection is not used any more. Nobody (the scheduler on a server no admin synchronised)
   * lends nothing.
   */
  mayLend(owner: string | null, isActiveAdmin: boolean): owner is string {
    return owner !== null && isActiveAdmin;
  },

  /**
   * A missing or expired connection needs the admin to connect; an OAuth provider that could not
   * be reached to renew it is `credentials_unavailable`, not the admin's to fix.
   */
  failureOf(failure: PersonalCredentialFailure): SyncFailure {
    return failure === 'unavailable' ? 'credentials_unavailable' : 'needs_connection';
  },
};

/** The text stored in `last_sync_error` and shown to admins. No upstream details, no secrets. */
export function syncFailureText(failure: SyncFailure): string {
  return TEXT[failure];
}

/**
 * The outcome of one synchronisation attempt, as stored on the server. `startedAt` orders
 * attempts: an older attempt never overwrites a newer one. `syncedBy` is set only by a
 * successful run an admin started (it becomes the scheduler's credentials owner).
 */
export interface SyncRecord {
  startedAt: Date;
  error: string | null;
  syncedBy: string | null;
}

export const SyncRecord = {
  success(startedBy: string | null, startedAt: Date): SyncRecord {
    return { startedAt, error: null, syncedBy: startedBy };
  },
  failure(failure: SyncFailure, startedAt: Date): SyncRecord {
    return { startedAt, error: syncFailureText(failure), syncedBy: null };
  },
};
