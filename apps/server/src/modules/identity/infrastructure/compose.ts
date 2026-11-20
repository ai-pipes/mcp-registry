import type { Sql } from '../../../platform/db.ts';
import type { IdentityDeps } from '../application/ports.ts';
import { cryptoRandom, nodeSha256 } from './crypto.ts';
import {
  PgLoginLinkRepository,
  PgPendingLoginRepository,
  PgSessionRepository,
  PgTokenRepository,
  PgUserRepository,
} from './pg-repositories.ts';
import { FAILED_CALLBACK_AUDIT_LIMIT, FAILED_CALLBACK_WINDOW_SEC } from '../domain/login-flow.ts';
import { InMemoryRateLimiter } from './rate-limiter.ts';

/** Everything in IdentityDeps except the cross-cutting parts the container provides. */
export type PgIdentity = Omit<IdentityDeps, 'audit' | 'clock' | 'oidc' | 'policy'>;

export function createPgIdentity(sql: Sql): PgIdentity {
  return {
    users: new PgUserRepository(sql),
    tokens: new PgTokenRepository(sql),
    sessions: new PgSessionRepository(sql),
    pendingLogins: new PgPendingLoginRepository(sql),
    loginLinks: new PgLoginLinkRepository(sql),
    sha256: nodeSha256,
    random: cryptoRandom,
    tokenLimiter: new InMemoryRateLimiter(30, 60, 60),
    linkLimiter: new InMemoryRateLimiter(10, 60, 60),
    callbackLimiter: new InMemoryRateLimiter(
      FAILED_CALLBACK_AUDIT_LIMIT,
      FAILED_CALLBACK_WINDOW_SEC,
      FAILED_CALLBACK_WINDOW_SEC,
    ),
  };
}
