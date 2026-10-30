import type { AuditLog, Clock } from '../../../shared-kernel/index.ts';
import type { GroupPolicy, OidcClaims, Role, User } from '../domain/user.ts';

/**
 * Talks to the identity provider (OIDC): Authorization Code + PKCE. Both methods throw
 * `IdentityError('idp_unavailable')` while the provider cannot be reached (its configuration is
 * not known yet); the server itself, `/mcp` and token auth never wait for it.
 */
export interface OidcProvider {
  authorizationUrl(input: { state: string; codeChallenge: string; nonce: string }): Promise<URL>;
  /** Exchanges the code, checks the ID token (signature, nonce) and returns the claims. */
  exchange(input: {
    code: string;
    codeVerifier: string;
    nonce: string;
    /** The `iss` parameter of the callback, when the provider sent one (RFC 9207). */
    iss?: string;
  }): Promise<OidcClaims>;
}

export interface RateLimiter {
  hit(key: string): { allowed: boolean; retryAfterSec: number };
  /** Non-counting: is the key currently locked out? */
  check(key: string): { allowed: boolean; retryAfterSec: number };
  reset(key: string): void;
}

export interface Sha256 {
  /** Lower-case hex SHA-256 of a UTF-8 string. */
  hex(input: string): string;
  /** Unpadded base64url SHA-256 of a UTF-8 string (PKCE S256 challenge). */
  base64url(input: string): string;
  /** Constant-time comparison of two strings. */
  timingSafeEqual(a: string, b: string): boolean;
}

export interface TokenRecord {
  id: string;
  userId: string;
  name: string;
  prefix: string;
  hash: string;
  createdAt: Date;
  lastUsedAt: Date | null;
  revokedAt: Date | null;
}

export interface SessionRecord {
  idHash: string;
  userId: string;
  createdAt: Date;
  lastSeenAt: Date;
  expiresAt: Date;
}

/** A started, not yet completed login. One-time: `consume` removes it. */
export interface PendingLogin {
  state: string;
  codeVerifier: string;
  nonce: string;
  returnTo: string;
  expiresAt: Date;
}

export interface UserRepository {
  findById(id: string): Promise<User | null>;
  findBySub(sub: string): Promise<User | null>;
  /** Creates the person or refreshes name, email, groups and role; never touches `disabled`. */
  upsertFromLogin(input: { claims: OidcClaims; role: Role; at: Date }): Promise<User>;
  list(): Promise<User[]>;
  setDisabled(id: string, disabled: boolean): Promise<boolean>;
}

export interface TokenRepository {
  insert(token: Omit<TokenRecord, 'id' | 'lastUsedAt' | 'revokedAt'>): Promise<TokenRecord>;
  findByHash(hash: string): Promise<TokenRecord | null>;
  findById(id: string): Promise<TokenRecord | null>;
  listByUser(userId: string): Promise<TokenRecord[]>;
  revoke(id: string, at: Date): Promise<void>;
  revokeAllByUser(userId: string, at: Date): Promise<void>;
  touch(id: string, at: Date): Promise<void>;
}

export interface SessionRepository {
  insert(session: SessionRecord): Promise<void>;
  findByHash(idHash: string): Promise<SessionRecord | null>;
  touch(idHash: string, at: Date): Promise<void>;
  delete(idHash: string): Promise<void>;
  deleteByUser(userId: string): Promise<void>;
  /** Deletes every session past either cutoff (`deadSessionCutoffs`); returns how many. */
  deleteDead(cutoffs: { expiresAtOrBefore: Date; lastSeenBefore: Date }): Promise<number>;
}

export interface PendingLoginRepository {
  insert(pending: PendingLogin): Promise<void>;
  /** Atomically removes and returns the record; null if unknown or already consumed. */
  consume(state: string): Promise<PendingLogin | null>;
}

export interface IdentityDeps {
  users: UserRepository;
  tokens: TokenRepository;
  sessions: SessionRepository;
  pendingLogins: PendingLoginRepository;
  oidc: OidcProvider;
  policy: GroupPolicy;
  sha256: Sha256;
  tokenLimiter: RateLimiter;
  audit: AuditLog;
  clock: Clock;
  /** `n` random bytes as an unpadded base64url string. */
  random: { base64url(bytes: number): string };
}
