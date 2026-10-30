import type { Sql } from '../../../platform/db.ts';
import type { IdentityDeps } from '../application/ports.ts';
import { cryptoRandom, nodeSha256 } from './crypto.ts';
import {
  PgPendingLoginRepository,
  PgSessionRepository,
  PgTokenRepository,
  PgUserRepository,
} from './pg-repositories.ts';
import { InMemoryRateLimiter } from './rate-limiter.ts';

/** Everything in IdentityDeps except the cross-cutting parts the container provides. */
export type PgIdentity = Omit<IdentityDeps, 'audit' | 'clock' | 'oidc' | 'policy'>;

export function createPgIdentity(sql: Sql): PgIdentity {
  return {
    users: new PgUserRepository(sql),
    tokens: new PgTokenRepository(sql),
    sessions: new PgSessionRepository(sql),
    pendingLogins: new PgPendingLoginRepository(sql),
    sha256: nodeSha256,
    random: cryptoRandom,
    tokenLimiter: new InMemoryRateLimiter(30, 60, 60),
  };
}
