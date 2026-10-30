import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { ConnectionError } from '../domain/errors.ts';
import type { ConnectionsDeps, PersonalAuthKind, PersonalServer, Principal } from './ports.ts';

/** The server if the principal may use it and it takes this kind of personal credential. */
export async function usablePersonalServer(
  deps: Pick<ConnectionsDeps, 'servers'>,
  principal: Principal,
  slug: string,
  kind: PersonalAuthKind,
): Promise<Result<PersonalServer, ConnectionError>> {
  const servers = await deps.servers.personalServersFor(principal);
  const server = servers.find((s) => s.slug === slug);
  if (!server) return err(new ConnectionError('not_found', `server "${slug}" not found`));
  if (server.authKind !== kind) {
    const what = kind === 'oauth_user' ? 'OAuth' : 'personal access tokens';
    return err(new ConnectionError('invalid_auth_kind', `server "${slug}" does not use ${what}`));
  }
  return ok(server);
}
