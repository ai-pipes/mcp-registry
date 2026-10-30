import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { Connection, PatToken } from '../domain/connection.ts';
import { ConnectionError } from '../domain/errors.ts';
import { canComplete, pendingConnectExpiry, startedWithConfig } from '../domain/pending-connect.ts';
import { serverFingerprint } from '../domain/server-fingerprint.ts';
import type { ConnectionsDeps, Principal } from './ports.ts';
import { usablePersonalServer } from './servers.ts';

const notFound = (slug: string) =>
  err(new ConnectionError('not_found', `server "${slug}" not found`));
const invalidState = () =>
  err(new ConnectionError('invalid_state', 'connect attempt is unknown or expired'));

/** Begins Authorization Code + PKCE for an `oauth_user` server: the provider URL to send the person to. */
export async function startConnect(
  deps: ConnectionsDeps,
  cmd: { principal: Principal; server: string },
): Promise<Result<URL, ConnectionError>> {
  const usable = await usablePersonalServer(deps, cmd.principal, cmd.server, 'oauth_user');
  if (!usable.ok) return usable;
  const { config } = usable.value;
  if (!config.oauth) return notFound(cmd.server);
  const state = deps.pkce.randomToken();
  const codeVerifier = deps.pkce.randomToken();
  await deps.pending.insert({
    state,
    userId: cmd.principal.userId,
    server: cmd.server,
    codeVerifier,
    issuedFor: serverFingerprint(config),
    expiresAt: pendingConnectExpiry(deps.clock.now()),
  });
  return ok(
    deps.oauth.authorizeUrl(config.oauth, {
      redirectUri: deps.redirectUri,
      state,
      codeChallenge: deps.pkce.challenge(codeVerifier),
    }),
  );
}

/** Finishes the connect: one-time state of this person, then the code is exchanged for tokens. */
export async function completeConnect(
  deps: ConnectionsDeps,
  cmd: { principal: Principal; state: string; code: string },
): Promise<Result<{ server: string }, ConnectionError>> {
  const pending = await deps.pending.consume(cmd.state);
  if (!pending || !canComplete(pending, { userId: cmd.principal.userId, now: deps.clock.now() })) {
    return invalidState();
  }
  const usable = await usablePersonalServer(deps, cmd.principal, pending.server, 'oauth_user');
  if (!usable.ok) return usable;
  const { config } = usable.value;
  const issuedFor = serverFingerprint(config);
  if (!startedWithConfig(pending, issuedFor)) return invalidState();
  if (!config.oauth) return notFound(pending.server);
  const clientSecret = await deps.servers.clientSecret(pending.server);

  let tokens;
  try {
    tokens = await deps.oauth.exchangeCode(config.oauth, clientSecret, {
      code: cmd.code,
      codeVerifier: pending.codeVerifier,
      redirectUri: deps.redirectUri,
    });
  } catch {
    return err(new ConnectionError('exchange_failed', 'the provider rejected the authorization'));
  }
  const connection = Connection.connectOAuth({
    userId: cmd.principal.userId,
    server: pending.server,
    access: deps.secretBox.seal(tokens.accessToken),
    refresh: tokens.refreshToken === null ? null : deps.secretBox.seal(tokens.refreshToken),
    expiresAt: tokens.expiresAt,
    issuedFor,
    revokeUrl: config.oauth.revokeUrl ?? null,
    now: deps.clock.now(),
  });
  await deps.connections.withLock(connection.userId, connection.server, (_, save) =>
    save(connection),
  );
  await deps.audit.record({
    actorId: cmd.principal.userId,
    action: 'connection.connect',
    target: pending.server,
  });
  return ok({ server: pending.server });
}

/** Stores a personal access token for a `pat_user` server, replacing any earlier one. */
export async function savePat(
  deps: ConnectionsDeps,
  cmd: { principal: Principal; server: string; token: string },
): Promise<Result<void, ConnectionError>> {
  const usable = await usablePersonalServer(deps, cmd.principal, cmd.server, 'pat_user');
  if (!usable.ok) return usable;
  const token = PatToken.parse(cmd.token);
  if (!token.ok) return token;
  const connection = Connection.connectPat({
    userId: cmd.principal.userId,
    server: cmd.server,
    token: deps.secretBox.seal(token.value),
    issuedFor: serverFingerprint(usable.value.config),
    now: deps.clock.now(),
  });
  await deps.connections.withLock(connection.userId, connection.server, (_, save) =>
    save(connection),
  );
  await deps.audit.record({
    actorId: cmd.principal.userId,
    action: 'connection.pat_saved',
    target: cmd.server,
  });
  return ok(undefined);
}

/**
 * Deletes the person's connection. Works even when the server is no longer usable: one's own
 * credentials can always be removed. Revokes the token upstream when the OAuth client still has
 * the revoke URL the token was issued with (a changed or newly added one gets nothing); a failing
 * revoke does not keep the connection.
 */
export async function disconnect(
  deps: ConnectionsDeps,
  cmd: { principal: Principal; server: string },
): Promise<void> {
  const removed = await deps.connections.delete(cmd.principal.userId, cmd.server);
  if (!removed) return;
  await deps.audit.record({
    actorId: cmd.principal.userId,
    action: 'connection.disconnect',
    target: cmd.server,
  });
  if (removed.kind !== 'oauth' || !deps.oauth.revoke) return;
  const server = await deps.servers.config(cmd.server);
  // Only to the provider that issued the token: never to one an admin has put in its place.
  if (!server?.oauth || !removed.issuedFor(serverFingerprint(server))) return;
  if (!removed.mayRevokeAt(server.oauth.revokeUrl)) return;
  const clientSecret = await deps.servers.clientSecret(cmd.server);
  // Revoking the refresh token ends the whole grant at providers that support RFC 7009.
  const token = deps.secretBox.open(removed.refreshToken ?? removed.accessToken);
  try {
    await deps.oauth.revoke(server.oauth, clientSecret, token);
  } catch {
    // The local credentials are gone already; the upstream token runs out on its own.
  }
}
