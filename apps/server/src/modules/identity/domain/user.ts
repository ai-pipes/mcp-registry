import { DomainError } from '../../../shared-kernel/index.ts';

export type Role = 'admin' | 'member';

/** Who is making a request: a person with a session, or the owner of an `sk_…` token. */
export interface Principal {
  userId: string;
  name: string;
  role: Role;
  /** Groups as of the person's last login. */
  groups: string[];
  via: 'session' | 'token';
  /** Set when `via` is `token`. */
  tokenId?: string;
}

/** What the identity provider tells us about a person. */
export interface OidcClaims {
  sub: string;
  name: string;
  email: string | null;
  groups: string[];
}

export interface User {
  id: string;
  oidcSub: string;
  name: string;
  email: string | null;
  groups: string[];
  role: Role;
  disabled: boolean;
  createdAt: Date;
  lastLoginAt: Date | null;
}

/** Provider groups that open the door (`loginGroup`) and that make a person an admin. */
export interface GroupPolicy {
  loginGroup: string;
  adminGroup: string;
}

export type IdentityErrorCode =
  | 'invalid_state'
  | 'not_allowed'
  | 'disabled'
  | 'invalid_token'
  | 'rate_limited'
  | 'forbidden'
  | 'not_found'
  | 'invalid_input'
  /** The identity provider cannot be reached right now; try signing in again later. */
  | 'idp_unavailable';

export class IdentityError extends DomainError {
  constructor(
    override readonly code: IdentityErrorCode,
    message: string,
    readonly retryAfterSec?: number,
  ) {
    super(code, message);
    this.name = 'IdentityError';
  }
}

/** The role a person with these groups gets, or null when they may not sign in at all. */
export function accessFor(groups: readonly string[], policy: GroupPolicy): Role | null {
  if (groups.includes(policy.adminGroup)) return 'admin';
  if (groups.includes(policy.loginGroup)) return 'member';
  return null;
}

/**
 * Groups for a person the operator provisions without the provider (CLI, emergency access): the
 * groups of their last login, if any, with exactly one policy group — the admin group with
 * `admin`, else the login group. Their next real login replaces them with the provider's claim.
 */
export function provisionedGroups(
  existing: readonly string[] | null,
  opts: { admin: boolean },
  policy: GroupPolicy,
): string[] {
  const other = (existing ?? []).filter((g) => g !== policy.loginGroup && g !== policy.adminGroup);
  return [...other, opts.admin ? policy.adminGroup : policy.loginGroup];
}

export function isAdmin(principal: Principal): boolean {
  return principal.role === 'admin';
}

/** An admin as of their last login who has not been disabled since. */
export function isActiveAdmin(user: User | null): boolean {
  return user !== null && !user.disabled && user.role === 'admin';
}

/** Anyone may revoke their own token; admins may revoke anyone's. */
export function canRevokeToken(principal: Principal, tokenOwnerId: string): boolean {
  return principal.userId === tokenOwnerId || isAdmin(principal);
}

/** Why `principal` may not set the disabled flag of `targetId`, or null when allowed. */
export function disableDenial(
  principal: Principal,
  targetId: string,
  disabled: boolean,
): IdentityError | null {
  if (!isAdmin(principal)) return new IdentityError('forbidden', 'admin only');
  if (disabled && targetId === principal.userId) {
    return new IdentityError('forbidden', 'cannot disable yourself');
  }
  return null;
}

export function principalOf(user: User, via: 'session' | 'token', tokenId?: string): Principal {
  const principal: Principal = {
    userId: user.id,
    name: user.name,
    role: user.role,
    groups: user.groups,
    via,
  };
  if (tokenId !== undefined) principal.tokenId = tokenId;
  return principal;
}
