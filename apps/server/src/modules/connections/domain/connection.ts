import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { ConnectionError } from './errors.ts';

export type ConnectionKind = 'oauth' | 'pat';
export type ConnectionStatus = 'ready' | 'expired';

/**
 * How the token endpoint answered a refresh: its HTTP status and OAuth `error` code. Null when
 * there was no answer at all (timeout, DNS, connection refused).
 */
export type RefreshFailure = { status: number; error: string | null } | null;

/**
 * Whether a failed refresh means the person's grant is gone: the token endpoint answered with an
 * OAuth error (`invalid_grant`, or HTTP 400/401). A timeout, a network error or another 5xx says
 * nothing about the grant; the connection stays as it was and the caller may try again later.
 *
 * The gateway's own client credentials failing is not a refusal of the grant either: a wrong
 * client secret (`invalid_client`: HTTP 401 at YouTrack Hub, 400 at Gitea; or
 * `unauthorized_client` with 401) is the admin's to fix, and every connection works again once
 * it is fixed. Gitea's `unauthorized_client` with 400 (an unparseable refresh token) still is.
 */
export function isRefreshRejected(failure: RefreshFailure): boolean {
  if (failure === null) return false;
  if (failure.error === 'invalid_client') return false;
  if (failure.error === 'unauthorized_client' && failure.status === 401) return false;
  return failure.error === 'invalid_grant' || failure.status === 400 || failure.status === 401;
}

/**
 * What to do with a connection when its secret is needed:
 * - `use`: hand out the current secret;
 * - `refresh`: get a new access token with the refresh token first;
 * - `expire`: the token is unusable and cannot be refreshed, mark the connection expired;
 * - `expired`: already expired, only a new connect helps.
 */
export type CredentialPlan =
  | { action: 'use' }
  | { action: 'refresh'; refreshToken: Uint8Array }
  | { action: 'expire'; reason: ExpiryReason }
  | { action: 'expired' };

/** Why a ready connection turns expired. */
export type ExpiryReason =
  | 'token_expired'
  | 'config_changed'
  | 'refresh_rejected'
  | 'rejected_upstream';

/** Refresh this long before the access token runs out. */
const REFRESH_AHEAD_MS = 60_000;

/** Encrypted secrets (SecretBox): the aggregate never holds a token in plain text. */
export type SealedTokens = {
  access: Uint8Array;
  refresh: Uint8Array | null;
  expiresAt: Date | null;
};

export type ConnectionSnapshot = SealedTokens & {
  userId: string;
  server: string;
  /** `serverFingerprint` of the server config the credential was issued for. */
  issuedFor: string;
  /**
   * The OAuth revoke URL in place when the tokens were issued (null: none then, or a PAT). On
   * disconnect the token goes only to this URL, never to one an admin put in its place later.
   */
  revokeUrl: string | null;
  kind: ConnectionKind;
  status: ConnectionStatus;
  updatedAt: Date;
};

/** A person's credentials to one server: an OAuth token pair or a PAT. */
export class Connection {
  private constructor(private s: ConnectionSnapshot) {}

  static connectOAuth(
    input: SealedTokens & {
      userId: string;
      server: string;
      issuedFor: string;
      revokeUrl: string | null;
      now: Date;
    },
  ) {
    return new Connection({
      userId: input.userId,
      server: input.server,
      issuedFor: input.issuedFor,
      revokeUrl: input.revokeUrl,
      kind: 'oauth',
      access: input.access,
      refresh: input.refresh,
      expiresAt: input.expiresAt,
      status: 'ready',
      updatedAt: input.now,
    });
  }

  /** A PAT has no expiry the gateway knows of and nothing to refresh it with. */
  static connectPat(input: {
    userId: string;
    server: string;
    token: Uint8Array;
    issuedFor: string;
    now: Date;
  }) {
    return new Connection({
      userId: input.userId,
      server: input.server,
      issuedFor: input.issuedFor,
      revokeUrl: null,
      kind: 'pat',
      access: input.token,
      refresh: null,
      expiresAt: null,
      status: 'ready',
      updatedAt: input.now,
    });
  }

  static restore(snapshot: ConnectionSnapshot): Connection {
    return new Connection({ ...snapshot });
  }

  get userId(): string {
    return this.s.userId;
  }
  get server(): string {
    return this.s.server;
  }
  get kind(): ConnectionKind {
    return this.s.kind;
  }
  get status(): ConnectionStatus {
    return this.s.status;
  }
  get accessToken(): Uint8Array {
    return this.s.access;
  }
  get refreshToken(): Uint8Array | null {
    return this.s.refresh;
  }
  get expiresAt(): Date | null {
    return this.s.expiresAt;
  }
  get updatedAt(): Date {
    return this.s.updatedAt;
  }
  get revokeUrl(): string | null {
    return this.s.revokeUrl;
  }

  snapshot(): ConnectionSnapshot {
    return { ...this.s };
  }

  /**
   * The rule: an OAuth token is refreshed ahead of time (less than 60 s left) or when
   * upstream rejected it (`forceRefresh`); without a refresh token it is used while it lasts. An
   * expired connection stays expired until the person connects again. A PAT is used as is: when
   * upstream rejects it, the gateway marks it expired. `config` is the fingerprint of the
   * server's config now: a credential issued for another config never leaves (it expires).
   */
  credentialPlan(now: Date, opts: { forceRefresh: boolean; config: string }): CredentialPlan {
    if (this.s.status === 'expired') return { action: 'expired' };
    if (!this.issuedFor(opts.config)) return { action: 'expire', reason: 'config_changed' };
    if (this.s.kind === 'pat') return { action: 'use' };
    const left = this.s.expiresAt === null ? Infinity : this.s.expiresAt.getTime() - now.getTime();
    if (!opts.forceRefresh && left >= REFRESH_AHEAD_MS) return { action: 'use' };
    if (this.s.refresh !== null) return { action: 'refresh', refreshToken: this.s.refresh };
    return !opts.forceRefresh && left > 0
      ? { action: 'use' }
      : { action: 'expire', reason: 'token_expired' };
  }

  /**
   * The provider could not be reached to refresh: the current access token may still be handed
   * out while it has not run out — unless upstream just rejected it (`forceRefresh`).
   */
  usableWhileProviderDown(now: Date, opts: { forceRefresh: boolean }): boolean {
    if (this.s.status === 'expired' || opts.forceRefresh) return false;
    return this.s.expiresAt === null || now.getTime() < this.s.expiresAt.getTime();
  }

  /** Whether the credential was issued for the server config with this fingerprint. */
  issuedFor(config: string): boolean {
    return this.s.issuedFor === config;
  }

  /**
   * Whether disconnect may send this connection's token to `currentRevokeUrl` (the server's
   * revoke URL now): only an OAuth token, and only to the revoke URL in place when it was issued.
   */
  mayRevokeAt(currentRevokeUrl: string | undefined): boolean {
    return (
      this.s.kind === 'oauth' &&
      this.s.revokeUrl !== null &&
      currentRevokeUrl !== undefined &&
      this.s.revokeUrl === currentRevokeUrl
    );
  }

  /** The status a person sees: a credential for an older server config counts as expired. */
  statusFor(config: string): ConnectionStatus {
    return this.issuedFor(config) ? this.s.status : 'expired';
  }

  /** New tokens from a refresh. A provider that does not rotate refresh tokens sends none back. */
  refreshed(tokens: SealedTokens, now: Date): void {
    this.s = {
      ...this.s,
      access: tokens.access,
      refresh: tokens.refresh ?? this.s.refresh,
      expiresAt: tokens.expiresAt,
      status: 'ready',
      updatedAt: now,
    };
  }

  /**
   * The same tokens sealed under another key (SECRETS_KEY rotation): not a new credential, so
   * status, expiry and `updatedAt` stay. Adding or dropping a refresh token is not a re-seal.
   */
  replaceSealed(tokens: { access: Uint8Array; refresh: Uint8Array | null }): void {
    if ((tokens.refresh === null) !== (this.s.refresh === null)) {
      throw new Error('a re-seal keeps the refresh token present or absent');
    }
    this.s = { ...this.s, access: tokens.access, refresh: tokens.refresh };
  }

  /** ready → expired: the token ran out, the refresh was rejected, upstream rejected the PAT. */
  expire(now: Date): void {
    this.s = { ...this.s, status: 'expired', updatedAt: now };
  }

  /** Changed since `seen` was read, e.g. refreshed by another process. */
  isNewerThan(seen: Connection): boolean {
    return this.s.updatedAt.getTime() > seen.updatedAt.getTime();
  }
}

const MAX_PAT_LENGTH = 4096;

export const PatToken = {
  /** A personal access token as pasted by a person: trimmed, one line, not empty. */
  parse(raw: string): Result<string, ConnectionError> {
    const token = raw.trim();
    if (token.length === 0 || token.length > MAX_PAT_LENGTH || /[\r\n\0]/.test(token)) {
      return err(new ConnectionError('invalid_pat', 'the token must be a non-empty single line'));
    }
    return ok(token);
  },
};
