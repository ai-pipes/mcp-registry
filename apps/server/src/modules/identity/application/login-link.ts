import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import {
  isLoginLinkExpired,
  looksLikeLoginCode,
  loginLinkExpiry,
  newLoginCode,
} from '../domain/login-link.ts';
import { sessionExpiry } from '../domain/session.ts';
import { IdentityError } from '../domain/user.ts';
import type { IdentityDeps } from './ports.ts';
import { provisionUser } from './users.ts';

/** The sub of the administrator created for a server that has no OIDC provider. */
export const LOCAL_ADMIN_SUB = 'local-admin';

/** Issues a one-time sign-in link for an existing, enabled person. Only the code's hash is kept. */
export async function issueLoginLink(
  deps: IdentityDeps,
  input: { sub: string },
): Promise<Result<{ code: string; expiresAt: Date }, IdentityError>> {
  const user = await deps.users.findBySub(input.sub);
  if (!user) return err(new IdentityError('not_found', `no user with sub "${input.sub}"`));
  if (user.disabled) return err(new IdentityError('disabled', 'account is disabled'));
  const now = deps.clock.now();
  const code = newLoginCode(deps.random.base64url(32));
  const expiresAt = loginLinkExpiry(now);
  await deps.loginLinks.insert({
    codeHash: deps.sha256.hex(code),
    userId: user.id,
    createdAt: now,
    expiresAt,
  });
  await deps.audit.record({ actorId: null, action: 'auth.link_issued', target: user.id });
  return ok({ code, expiresAt });
}

/**
 * Redeems a link once. Unknown, expired and already used codes are all `invalid_link`. Failures
 * (and only those) are counted per IP; a valid code is still accepted from a locked-out IP.
 */
export async function completeLinkLogin(
  deps: IdentityDeps,
  input: { code: string; ip: string },
): Promise<Result<{ sessionId: string }, IdentityError>> {
  // Only a link that was actually consumed and then refused is audited; unknown or malformed
  // codes write nothing, so an unauthenticated caller cannot grow the audit log.
  const reject = async (
    reason: 'invalid_link' | 'disabled',
    consumed: { actorId: string | null } | null,
  ): Promise<Result<{ sessionId: string }, IdentityError>> => {
    if (consumed) {
      await deps.audit.record({
        actorId: consumed.actorId,
        action: 'auth.login_failed',
        meta: { reason },
      });
    }
    const hit = deps.linkLimiter.hit(input.ip);
    if (!hit.allowed) {
      return err(new IdentityError('rate_limited', 'too many failed attempts', hit.retryAfterSec));
    }
    return err(
      reason === 'disabled'
        ? new IdentityError('disabled', 'account is disabled')
        : new IdentityError('invalid_link', 'the link is invalid or has expired'),
    );
  };

  if (!looksLikeLoginCode(input.code)) return reject('invalid_link', null);
  const now = deps.clock.now();
  const link = await deps.loginLinks.consume(deps.sha256.hex(input.code));
  if (!link) return reject('invalid_link', null);
  if (isLoginLinkExpired(link.expiresAt, now)) return reject('invalid_link', { actorId: null });
  const user = await deps.users.findById(link.userId);
  if (!user) return reject('invalid_link', { actorId: null });
  if (user.disabled) return reject('disabled', { actorId: user.id });

  const sessionId = deps.random.base64url(32);
  await deps.sessions.insert({
    idHash: deps.sha256.hex(sessionId),
    userId: user.id,
    createdAt: now,
    lastSeenAt: now,
    expiresAt: sessionExpiry(now),
  });
  await deps.users.touchLastLogin(user.id, now);
  await deps.audit.record({ actorId: user.id, action: 'auth.login', meta: { via: 'link' } });
  return ok({ sessionId });
}

/**
 * For a server without OIDC: issues a link for `local-admin`, creating it when there are no people
 * at all. Also issues a new link while `local-admin` is the only person and has never signed in,
 * so a failed first start cannot lock the install out. Otherwise returns null.
 */
export async function bootstrapLocalAdmin(deps: IdentityDeps): Promise<{ code: string } | null> {
  const people = await deps.users.list();
  const [only] = people;
  const pending =
    people.length === 1 &&
    only?.oidcSub === LOCAL_ADMIN_SUB &&
    !only.disabled &&
    // Provisioning stamps lastLoginAt = createdAt; any real sign-in moves it later.
    (only.lastLoginAt === null || only.lastLoginAt.getTime() <= only.createdAt.getTime());
  if (people.length > 0 && !pending) return null;
  if (people.length === 0) {
    const created = await provisionUser(deps, {
      sub: LOCAL_ADMIN_SUB,
      name: 'Admin',
      admin: true,
    });
    if (!created.ok) throw created.error;
  }
  const link = await issueLoginLink(deps, { sub: LOCAL_ADMIN_SUB });
  return link.ok ? { code: link.value.code } : null;
}
