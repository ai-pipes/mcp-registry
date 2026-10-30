// Public API of the identity module: the only file other modules and the platform may import.
export { authenticateSession, logout, pruneExpiredSessions } from './application/sessions.ts';
export { authenticateToken, createToken, listTokens, revokeToken } from './application/tokens.ts';
export type { TokenSummary } from './application/tokens.ts';
export {
  actAs,
  isUserActiveAdmin,
  listUsers,
  provisionUser,
  setDisabled,
} from './application/users.ts';
export type { UserSummary } from './application/users.ts';
export { completeOidcLogin, startOidcLogin } from './application/oidc-login.ts';
export type { IdentityDeps, OidcProvider } from './application/ports.ts';
export { IdentityError } from './domain/user.ts';
export type { GroupPolicy, IdentityErrorCode, OidcClaims, Principal, Role } from './domain/user.ts';
export { createPgIdentity } from './infrastructure/compose.ts';
export type { PgIdentity } from './infrastructure/compose.ts';
export { OpenidProvider } from './infrastructure/openid-provider.ts';
export { identityRoutes } from './interfaces/http/routes.ts';
export {
  requireAdminRole,
  requireBearerToken,
  requireSession,
  sessionPrincipal,
} from './interfaces/http/session.ts';
export type { IdentityEnv } from './interfaces/http/shared.ts';
