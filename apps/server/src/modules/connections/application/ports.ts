import type { AuditLog, Clock, Result, SecretBox } from '../../../shared-kernel/index.ts';
import type { AuthMode, OAuthClientConfig } from '../../catalog/index.ts';
import type { Principal } from '../../identity/index.ts';
import type { Connection } from '../domain/connection.ts';
import type { PendingConnect } from '../domain/pending-connect.ts';

export type { Principal };

/** What the OAuth provider gave back. `expiresAt` is null when it did not say. */
export type TokenSet = { accessToken: string; refreshToken: string | null; expiresAt: Date | null };

/**
 * The token endpoint answered a request with an error: its HTTP status and the OAuth `error` code
 * when the body had one. `OAuthClient` throws this for such answers; anything else it throws means
 * there was no usable answer (timeout, network, a malformed response).
 */
export class TokenEndpointError extends Error {
  constructor(
    readonly status: number,
    readonly error: string | null,
  ) {
    super(`token endpoint answered HTTP ${status}${error === null ? '' : ` (${error})`}`);
    this.name = 'TokenEndpointError';
  }
}

/** Authorization Code + PKCE (S256) against the upstream's OAuth provider. */
export interface OAuthClient {
  authorizeUrl(
    cfg: OAuthClientConfig,
    input: { redirectUri: string; state: string; codeChallenge: string },
  ): URL;
  /** `clientSecret` is null for a public client. Throws when the provider refuses (see TokenEndpointError). */
  exchangeCode(
    cfg: OAuthClientConfig,
    clientSecret: string | null,
    input: { code: string; codeVerifier: string; redirectUri: string },
  ): Promise<TokenSet>;
  /** Throws TokenEndpointError for an error answer, anything else when there was no answer. */
  refresh(
    cfg: OAuthClientConfig,
    clientSecret: string | null,
    refreshToken: string,
  ): Promise<TokenSet>;
  /** RFC 7009; only called when the config has a `revokeUrl`. */
  revoke?(cfg: OAuthClientConfig, clientSecret: string | null, token: string): Promise<void>;
}

export type PersonalAuthKind = 'oauth_user' | 'pat_user';

/** An active server's config as far as personal credentials care. No secret. */
export type ServerConfig = {
  slug: string;
  url: string;
  authKind: AuthMode['kind'];
  /** Set exactly for `oauth_user`. */
  oauth: OAuthClientConfig | null;
};

/** A server that takes a person's own credentials. */
export type PersonalServer = {
  slug: string;
  name: string;
  authKind: PersonalAuthKind;
  config: ServerConfig;
};

/** Read access to the catalog, implemented over its public API. */
export interface ServerDirectory {
  /** Active `oauth_user`/`pat_user` servers the principal may use, ordered by slug. */
  personalServersFor(principal: Principal): Promise<PersonalServer[]>;
  /** The config of an active server; null when unknown or disabled. */
  config(slug: string): Promise<ServerConfig | null>;
  /** The server's OAuth client secret, decrypted; null when unset. Never leaves the module. */
  clientSecret(slug: string): Promise<string | null>;
}

/** Persisted connections. Tokens arrive and leave sealed (the aggregate holds no plain text). */
export interface ConnectionStore {
  get(userId: string, server: string): Promise<Connection | null>;
  listForUser(userId: string): Promise<Connection[]>;
  /**
   * Runs `fn` while holding the (userId, server) connection exclusively — across processes, not
   * only in this one (Postgres: `select … for update` in a transaction). `save` writes inside it.
   * `fn` must not use the database otherwise (catalog, audit): the lock holds one pool connection,
   * and waiting for a second one under it can starve the pool. Read before, write audit after.
   */
  withLock<T>(
    userId: string,
    server: string,
    fn: (current: Connection | null, save: (c: Connection) => Promise<void>) => Promise<T>,
  ): Promise<T>;
  /** Removes the connection and returns what was there. */
  delete(userId: string, server: string): Promise<Connection | null>;
  /** Who is connected to what: every stored connection's key (SECRETS_KEY rotation). */
  listKeys(): Promise<{ userId: string; server: string }[]>;
}

/** Started connect attempts. One-time: `consume` removes the attempt it returns. */
export interface PendingConnects {
  insert(pending: PendingConnect): Promise<void>;
  consume(state: string): Promise<PendingConnect | null>;
}

export interface Pkce {
  /** A random, URL-safe string: state and code verifier. */
  randomToken(): string;
  /** `BASE64URL(SHA-256(verifier))` (RFC 7636 S256). */
  challenge(verifier: string): string;
}

/** Joins concurrent calls with the same key into one run inside this process. */
export interface SingleFlight<T> {
  run(key: string, fn: () => Promise<T>): Promise<T>;
}

/**
 * Why there is no usable personal secret: never connected (or the server is gone), connected but
 * expired (connect again), or the OAuth provider could not be reached to refresh (try later).
 */
export type CredentialFailure = 'needs_connection' | 'expired' | 'unavailable';

export type ConnectionsDeps = {
  connections: ConnectionStore;
  pending: PendingConnects;
  servers: ServerDirectory;
  oauth: OAuthClient;
  pkce: Pkce;
  secretBox: SecretBox;
  /** One credential renewal per (userId, server) at a time in this process. */
  flights: SingleFlight<Result<string, CredentialFailure>>;
  clock: Clock;
  audit: AuditLog;
  /** `<PUBLIC_URL>/api/connections/callback`. */
  redirectUri: string;
};
