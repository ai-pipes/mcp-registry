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
  const reject = async (
    reason: 'invalid_link' | 'disabled',
    actorId: string | null,
  ): Promise<Result<{ sessionId: string }, IdentityError>> => {
    await deps.audit.record({ actorId, action: 'auth.login_failed', meta: { reason } });
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
  if (!link || isLoginLinkExpired(link.expiresAt, now)) return reject('invalid_link', null);
  const user = await deps.users.findById(link.userId);
  if (!user) return reject('invalid_link', null);
  if (user.disabled) return reject('disabled', user.id);

  const sessionId = deps.random.base64url(32);
  await deps.sessions.insert({
    idHash: deps.sha256.hex(sessionId),
    userId: user.id,
    createdAt: now,
    lastSeenAt: now,
    expiresAt: sessionExpiry(now),
  });
  await deps.audit.record({ actorId: user.id, action: 'auth.login', meta: { via: 'link' } });
  return ok({ sessionId });
}

/**
 * For a server without OIDC: when there are no people at all, creates the admin `local-admin` and
 * a link for them. Returns null when anyone exists (even a disabled person).
 */
export async function bootstrapLocalAdmin(deps: IdentityDeps): Promise<{ code: string } | null> {
  if ((await deps.users.list()).length > 0) return null;
  const created = await provisionUser(deps, { sub: LOCAL_ADMIN_SUB, name: 'Admin', admin: true });
  if (!created.ok) return null;
  const link = await issueLoginLink(deps, { sub: LOCAL_ADMIN_SUB });
  return link.ok ? { code: link.value.code } : null;
}
