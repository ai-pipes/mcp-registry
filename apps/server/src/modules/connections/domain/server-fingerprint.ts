/** What a person's credential was issued for: where it is sent and who issued it. */
export type ServerConfigOrigin = {
  url: string;
  authKind: string;
  /** `revokeUrl` may be present; it is not part of the fingerprint. */
  oauth: { authorizeUrl: string; tokenUrl: string; clientId: string; revokeUrl?: string } | null;
};

/**
 * A canonical string of the server config a connection is bound to: the upstream URL, the auth
 * kind and, for OAuth, the authorize and token URLs and the client id. Every one of them is a
 * place the person's token (or code) is sent on calls and refreshes. When an admin changes any of
 * them, the token must not follow to the new place, so the connection expires. Scopes are left
 * out. The revoke URL is not part of it: only disconnect sends a token there, and the connection
 * keeps the revoke URL it was issued with (`Connection.mayRevokeAt`), so turning revocation on or
 * changing its URL expires nobody.
 */
export function serverFingerprint(cfg: ServerConfigOrigin): string {
  return JSON.stringify([
    cfg.url,
    cfg.authKind,
    cfg.oauth?.authorizeUrl ?? null,
    cfg.oauth?.tokenUrl ?? null,
    cfg.oauth?.clientId ?? null,
  ]);
}
