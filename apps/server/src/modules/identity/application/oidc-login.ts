import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { isLoginStateExpired, loginStateExpiry, normalizeReturnTo } from '../domain/login-flow.ts';
import { sessionExpiry } from '../domain/session.ts';
import { accessFor, IdentityError } from '../domain/user.ts';
import type { IdentityDeps } from './ports.ts';

const randomToken = (deps: IdentityDeps, bytes: number): string => deps.random.base64url(bytes);

/**
 * Begins Authorization Code + PKCE: returns the provider URL and remembers the attempt.
 * `idp_unavailable` when the provider cannot be reached (nothing is remembered then).
 */
export async function startOidcLogin(
  deps: IdentityDeps,
  input: { returnTo: string },
): Promise<Result<URL, IdentityError>> {
  const state = randomToken(deps, 32);
  const nonce = randomToken(deps, 32);
  const codeVerifier = randomToken(deps, 32);
  let url: URL;
  try {
    url = await deps.oidc.authorizationUrl({
      state,
      nonce,
      codeChallenge: deps.sha256.base64url(codeVerifier),
    });
  } catch (error) {
    if (isIdpUnavailable(error)) return err(error);
    throw error;
  }
  await deps.pendingLogins.insert({
    state,
    codeVerifier,
    nonce,
    returnTo: normalizeReturnTo(input.returnTo),
    expiresAt: loginStateExpiry(deps.clock.now()),
  });
  return ok(url);
}

const isIdpUnavailable = (error: unknown): error is IdentityError =>
  error instanceof IdentityError && error.code === 'idp_unavailable';

/** Finishes the login: checks the state, the provider's answer and the group policy. */
export async function completeOidcLogin(
  deps: IdentityDeps,
  input: { state: string; code: string; iss?: string },
): Promise<Result<{ sessionId: string; returnTo: string }, IdentityError>> {
  const pending = await deps.pendingLogins.consume(input.state);
  if (!pending || isLoginStateExpired(pending.expiresAt, deps.clock.now())) {
    return err(new IdentityError('invalid_state', 'login attempt is unknown or expired'));
  }

  let claims;
  try {
    claims = await deps.oidc.exchange({
      code: input.code,
      codeVerifier: pending.codeVerifier,
      nonce: pending.nonce,
      ...(input.iss === undefined ? {} : { iss: input.iss }),
    });
  } catch (error) {
    if (isIdpUnavailable(error)) return err(error);
    await deps.audit.record({
      actorId: null,
      action: 'auth.login_failed',
      meta: { reason: 'exchange_failed' },
    });
    return err(new IdentityError('invalid_state', 'the identity provider rejected the login'));
  }

  const role = accessFor(claims.groups, deps.policy);
  if (!role) {
    await deps.audit.record({
      actorId: null,
      action: 'auth.login_failed',
      meta: { reason: 'not_allowed', sub: claims.sub },
    });
    return err(new IdentityError('not_allowed', 'your account is not in an allowed group'));
  }

  const existing = await deps.users.findBySub(claims.sub);
  if (existing?.disabled) {
    await deps.audit.record({
      actorId: existing.id,
      action: 'auth.login_failed',
      meta: { reason: 'disabled' },
    });
    return err(new IdentityError('disabled', 'account is disabled'));
  }

  const now = deps.clock.now();
  const user = await deps.users.upsertFromLogin({ claims, role, at: now });
  const sessionId = randomToken(deps, 32);
  await deps.sessions.insert({
    idHash: deps.sha256.hex(sessionId),
    userId: user.id,
    createdAt: now,
    lastSeenAt: now,
    expiresAt: sessionExpiry(now),
  });
  await deps.audit.record({ actorId: user.id, action: 'auth.login' });
  return ok({ sessionId, returnTo: pending.returnTo });
}
