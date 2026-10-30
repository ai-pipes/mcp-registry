import { Hono } from 'hono';
import type { IdentityDeps } from '../../application/ports.ts';
import { authRoutes, type BrowserRouteOptions } from './auth.ts';
import type { IdentityEnv } from './shared.ts';
import { tokenRoutes } from './tokens.ts';
import { userRoutes } from './users.ts';

/** Everything identity serves under `/api`: auth, tokens, users. */
export function identityRoutes(deps: IdentityDeps, opts: BrowserRouteOptions) {
  return new Hono<IdentityEnv>()
    .route('/auth', authRoutes(deps, opts))
    .route('/tokens', tokenRoutes(deps))
    .route('/users', userRoutes(deps));
}
