import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import {
  accessFor,
  disableDenial,
  IdentityError,
  isActiveAdmin,
  isAdmin,
  type Principal,
  principalOf,
  provisionedGroups,
  type Role,
} from '../domain/user.ts';
import type { IdentityDeps } from './ports.ts';

export interface UserSummary {
  id: string;
  name: string;
  email: string | null;
  role: Role;
  groups: string[];
  disabled: boolean;
  createdAt: Date;
  lastLoginAt: Date | null;
}

function requireAdmin(principal: Principal): IdentityError | null {
  return isAdmin(principal) ? null : new IdentityError('forbidden', 'admin only');
}

export async function listUsers(
  deps: IdentityDeps,
  input: { principal: Principal },
): Promise<Result<UserSummary[], IdentityError>> {
  const denied = requireAdmin(input.principal);
  if (denied) return err(denied);
  const rows = await deps.users.list();
  return ok(
    rows.map((u) => ({
      id: u.id,
      name: u.name,
      email: u.email,
      role: u.role,
      groups: u.groups,
      disabled: u.disabled,
      createdAt: u.createdAt,
      lastLoginAt: u.lastLoginAt,
    })),
  );
}

/** Disabling also ends every session and revokes every token of the person. */
export async function setDisabled(
  deps: IdentityDeps,
  input: { principal: Principal; userId: string; disabled: boolean },
): Promise<Result<void, IdentityError>> {
  const denied = disableDenial(input.principal, input.userId, input.disabled);
  if (denied) return err(denied);
  const found = await deps.users.setDisabled(input.userId, input.disabled);
  if (!found) return err(new IdentityError('not_found', 'user not found'));
  if (input.disabled) {
    await deps.sessions.deleteByUser(input.userId);
    await deps.tokens.revokeAllByUser(input.userId, deps.clock.now());
  }
  await deps.audit.record({
    actorId: input.principal.userId,
    action: 'user.disable',
    target: input.userId,
    meta: { disabled: input.disabled },
  });
  return ok(undefined);
}

/**
 * Whether the user is still an enabled admin (role as of their last login). For work done on an
 * admin's behalf without a session, e.g. scheduled catalog synchronisation with their connection.
 */
export async function isUserActiveAdmin(
  deps: Pick<IdentityDeps, 'users'>,
  userId: string,
): Promise<boolean> {
  return isActiveAdmin(await deps.users.findById(userId));
}

/**
 * Operator provisioning (CLI): creates the person as their first OIDC login would, with the login
 * group or, with `admin`, the admin group. Idempotent by `sub`: an existing person keeps their id
 * and other groups. A disabled person stays disabled (re-enable them in the UI).
 */
export async function provisionUser(
  deps: IdentityDeps,
  cmd: { sub: string; name: string; admin: boolean },
): Promise<Result<{ id: string; role: Role; created: boolean }, IdentityError>> {
  const sub = cmd.sub.trim();
  const name = cmd.name.trim();
  if (sub === '' || name === '') {
    return err(new IdentityError('invalid_input', 'sub and name must not be blank'));
  }
  const existing = await deps.users.findBySub(sub);
  if (existing?.disabled) return err(new IdentityError('disabled', 'account is disabled'));
  const groups = provisionedGroups(existing?.groups ?? null, cmd, deps.policy);
  const role = accessFor(groups, deps.policy) ?? 'member';
  const user = await deps.users.upsertFromLogin({
    claims: { sub, name, email: existing?.email ?? null, groups },
    role,
    at: deps.clock.now(),
  });
  await deps.audit.record({
    actorId: null,
    action: 'user.provision',
    target: user.id,
    meta: { via: 'cli', role, created: existing === null },
  });
  return ok({ id: user.id, role: user.role, created: existing === null });
}

/** The principal of an enabled person, for operator commands (CLI) run on their behalf. */
export async function actAs(
  deps: Pick<IdentityDeps, 'users'>,
  sub: string,
): Promise<Result<Principal, IdentityError>> {
  const user = await deps.users.findBySub(sub);
  if (!user) return err(new IdentityError('not_found', `no user with sub "${sub}"`));
  if (user.disabled) return err(new IdentityError('disabled', 'account is disabled'));
  return ok(principalOf(user, 'session'));
}
