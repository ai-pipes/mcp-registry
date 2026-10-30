import type { Context, MiddlewareHandler } from 'hono';
import { deleteCookie, getCookie, setCookie } from 'hono/cookie';
import { domainErrorResponse, errorResponse } from '../../../../platform/http/errors.ts';
import type { IdentityDeps } from '../../application/ports.ts';
import { authenticateSession } from '../../application/sessions.ts';
import { authenticateToken } from '../../application/tokens.ts';
import type { Principal } from '../../domain/user.ts';
import type { IdentityEnv } from './shared.ts';

const SESSION_COOKIE = 'mcp_session';
const MAX_SESSION_ID_LENGTH = 64;

const cookieOptions = (secure: boolean) =>
  ({ httpOnly: true, sameSite: 'Strict', path: '/', secure }) as const;

export function setSessionCookie(c: Context, sessionId: string, secure: boolean): void {
  setCookie(c, SESSION_COOKIE, sessionId, cookieOptions(secure));
}

export function clearSessionCookie(c: Context, secure: boolean): void {
  deleteCookie(c, SESSION_COOKIE, cookieOptions(secure));
}

const LOGIN_COOKIE = 'mcp_login';
const LOGIN_COOKIE_MAX_AGE_SEC = 600;
// Lax: the callback is a cross-site top-level redirect from the provider; Strict would drop it.
const loginCookieOptions = (secure: boolean) =>
  ({ httpOnly: true, sameSite: 'Lax', path: '/api/auth', secure }) as const;

export function setLoginBindingCookie(c: Context, state: string, secure: boolean): void {
  setCookie(c, LOGIN_COOKIE, state, {
    ...loginCookieOptions(secure),
    maxAge: LOGIN_COOKIE_MAX_AGE_SEC,
  });
}

export function clearLoginBindingCookie(c: Context, secure: boolean): void {
  deleteCookie(c, LOGIN_COOKIE, loginCookieOptions(secure));
}

export function loginBindingOf(c: Context): string | null {
  return getCookie(c, LOGIN_COOKIE) ?? null;
}

export function sessionIdOf(c: Context): string | null {
  const id = getCookie(c, SESSION_COOKIE);
  return id && id.length <= MAX_SESSION_ID_LENGTH ? id : null;
}

/**
 * The person behind the session cookie, or null, for /api routes that answer a missing session
 * with a redirect rather than 401 (provider callbacks). Like `requireSession`, a request with an
 * `Authorization` header gets no principal.
 */
export async function sessionPrincipal(deps: IdentityDeps, c: Context): Promise<Principal | null> {
  if (c.req.header('authorization') !== undefined) return null;
  const sessionId = sessionIdOf(c);
  return sessionId ? authenticateSession(deps, { sessionId }) : null;
}

/**
 * `/api/**`: the session cookie is the only credential. An `Authorization` header — a Bearer
 * `sk_…` token in particular — is never accepted here, even next to a valid cookie.
 */
export function requireSession(deps: IdentityDeps): MiddlewareHandler<IdentityEnv> {
  return async (c, next) => {
    if (c.req.header('authorization') !== undefined) {
      return errorResponse(c, 'unauthenticated', 'tokens do not work on the web API');
    }
    const principal = await sessionPrincipal(deps, c);
    if (!principal) return errorResponse(c, 'unauthenticated', 'sign in required');
    c.set('principal', principal);
    return next();
  };
}

const BEARER = /^Bearer[ \t]+(\S+)$/i;

/** `/mcp`: `Authorization: Bearer sk_…` is the only credential; the cookie is ignored. */
export function requireBearerToken(deps: IdentityDeps): MiddlewareHandler<IdentityEnv> {
  return async (c, next) => {
    const token = BEARER.exec(c.req.header('authorization') ?? '')?.[1];
    if (!token) {
      c.header('WWW-Authenticate', 'Bearer');
      return errorResponse(c, 'unauthenticated', 'bearer token required');
    }
    const auth = await authenticateToken(deps, { token, ip: c.get('clientIp') });
    if (!auth.ok) {
      if (auth.error.code !== 'rate_limited') c.header('WWW-Authenticate', 'Bearer');
      return domainErrorResponse(c, auth.error);
    }
    c.set('principal', auth.value);
    return next();
  };
}

/** After `requireSession`: only admins pass. Runs before body parsing, so a member learns nothing. */
export const requireAdminRole: MiddlewareHandler<IdentityEnv> = (c, next) =>
  c.get('principal').role === 'admin'
    ? next()
    : Promise.resolve(errorResponse(c, 'forbidden', 'admin only'));
