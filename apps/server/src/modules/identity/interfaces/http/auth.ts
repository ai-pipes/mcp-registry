import {
  type AuthMethods,
  CallbackQuery,
  LinkLoginBody,
  LoginQuery,
  type MeResponse,
} from '@mcp/contracts';
import { Hono } from 'hono';
import { readJson, readQuery } from '../../../../platform/http/request.ts';
import { completeLinkLogin } from '../../application/login-link.ts';
import { completeOidcLogin, startOidcLogin } from '../../application/oidc-login.ts';
import type { IdentityDeps } from '../../application/ports.ts';
import { logout } from '../../application/sessions.ts';
import {
  clearLoginBindingCookie,
  clearSessionCookie,
  loginBindingOf,
  requireSession,
  sessionIdOf,
  setLoginBindingCookie,
  setSessionCookie,
} from './session.ts';
import type { IdentityEnv } from './shared.ts';

/**
 * Options of routes a browser is sent to by another site (OAuth/OIDC callbacks): such a route
 * answers every outcome with a redirect to a UI page, so an unexpected error is logged here
 * (`logError` drops the message) and the page gets a fixed code.
 */
export type BrowserRouteOptions = {
  cookieSecure: boolean;
  /** Whether `OIDC_*` is set; without it people sign in with a one-time link. */
  oidcConfigured: boolean;
  logError: (error: unknown, message: string) => void;
};

/** Status of each way a link sign-in can fail: wrong code, refused person, too many tries. */
const LINK_ERROR_STATUS: Partial<Record<string, 401 | 403 | 429>> = {
  invalid_link: 401,
  disabled: 403,
  rate_limited: 429,
};

/** `/api/auth`: login and callback are the only /api routes reachable without a session. */
export function authRoutes(deps: IdentityDeps, opts: BrowserRouteOptions) {
  return new Hono<IdentityEnv>()
    .get('/methods', (c) => {
      const methods: AuthMethods = { oidc: opts.oidcConfigured };
      return c.json(methods);
    })
    .post('/link', async (c) => {
      const { code } = await readJson(c, LinkLoginBody);
      const r = await completeLinkLogin(deps, { code, ip: c.get('clientIp') });
      if (!r.ok) {
        if (r.error.code === 'rate_limited' && r.error.retryAfterSec !== undefined) {
          c.header('Retry-After', String(r.error.retryAfterSec));
        }
        // The message is fixed text: it never carries the code.
        return c.json(
          { error: { code: r.error.code, message: r.error.message } },
          LINK_ERROR_STATUS[r.error.code] ?? 400,
        );
      }
      // The person signed in with a link: the session this browser held is ended, not left alive.
      const replaced = sessionIdOf(c);
      if (replaced) await logout(deps, { sessionId: replaced });
      setSessionCookie(c, r.value.sessionId, opts.cookieSecure);
      return c.body(null, 204);
    })
    .get('/login', async (c) => {
      if (!opts.oidcConfigured) return c.redirect('/?loginError=oidc_not_configured', 302);
      const { returnTo } = readQuery(c, LoginQuery);
      const started = await startOidcLogin(deps, { returnTo });
      // The provider is unreachable: back to the UI with a fixed code, as the callback does.
      if (!started.ok) return c.redirect(`/?loginError=${started.error.code}`, 302);
      const url = started.value;
      // Binds this attempt to this browser: the callback must come back with the same cookie.
      setLoginBindingCookie(c, url.searchParams.get('state') ?? '', opts.cookieSecure);
      return c.redirect(url.toString(), 302);
    })
    .get('/callback', async (c) => {
      const parsed = CallbackQuery.safeParse(c.req.query());
      // A cancelled or broken provider answer carries no usable code/state.
      const bound = loginBindingOf(c);
      clearLoginBindingCookie(c, opts.cookieSecure);
      if (!parsed.success || bound !== parsed.data.state) {
        return c.redirect('/?loginError=invalid_state', 302);
      }
      let r;
      try {
        r = await completeOidcLogin(deps, {
          state: parsed.data.state,
          code: parsed.data.code,
          ...(parsed.data.iss === undefined ? {} : { iss: parsed.data.iss }),
        });
      } catch (error) {
        // The browser came back from the OIDC provider: a page with a fixed code, not a JSON 500.
        opts.logError(error, 'auth: login callback failed');
        return c.redirect('/?loginError=internal', 302);
      }
      if (!r.ok) return c.redirect(`/?loginError=${r.error.code}`, 302);
      setSessionCookie(c, r.value.sessionId, opts.cookieSecure);
      return c.redirect(r.value.returnTo, 302);
    })
    .post('/logout', async (c) => {
      const sessionId = sessionIdOf(c);
      if (sessionId) await logout(deps, { sessionId });
      clearSessionCookie(c, opts.cookieSecure);
      return c.body(null, 204);
    })
    .get('/me', requireSession(deps), (c) => {
      const p = c.get('principal');
      const me: MeResponse = { userId: p.userId, name: p.name, role: p.role, groups: p.groups };
      return c.json(me);
    });
}
