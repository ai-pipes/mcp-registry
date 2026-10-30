import { z } from 'zod';
import type { Result } from '../../../../shared-kernel/index.ts';
import type { AppEnv } from '../../../../platform/http/env.ts';
import { IdentityError, type Principal } from '../../domain/user.ts';

/** Variables of the platform plus the caller that the session middleware resolved. */
export type IdentityEnv = {
  Variables: AppEnv['Variables'] & { principal: Principal };
};

const Uuid = z.uuid();

/** Ids that are not UUIDs cannot exist; answering 404 keeps them away from Postgres. */
export function parseId(raw: string, what: string): string {
  if (!Uuid.safeParse(raw).success) throw new IdentityError('not_found', `${what} not found`);
  return raw;
}

/** The value of a successful result; an error is thrown for the app's error handler. */
export function unwrap<T, E extends Error>(r: Result<T, E>): T {
  if (!r.ok) throw r.error;
  return r.value;
}
