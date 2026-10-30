import { err, ok, type Result } from '../../../../shared-kernel/index.ts';
import { canUse } from '../../domain/access.ts';
import type { CatalogError } from '../../domain/errors.ts';
import { requireAdmin } from '../admin.ts';
import type { CatalogDeps } from '../deps.ts';
import type { MemberServerView, Principal, ServerView, ServerCallTarget } from '../ports.ts';
import { toMemberServerView, toServerView, toServerCallTarget } from '../views.ts';

/** Active servers this principal may use, in the member's (reduced) view. */
export async function listServersFor(
  deps: Pick<CatalogDeps, 'servers'>,
  q: { principal: Principal },
): Promise<MemberServerView[]> {
  const all = await deps.servers.list();
  return all
    .filter((l) => canUse(l.server, q.principal))
    .map((l) => toMemberServerView(l.server, l.toolCount));
}

/** Admin screen: every server, disabled ones included. */
export async function listAllServers(
  deps: Pick<CatalogDeps, 'servers'>,
  q: { principal: Principal },
): Promise<Result<ServerView[], CatalogError>> {
  const admin = requireAdmin(q.principal);
  if (!admin.ok) return err(admin.error);
  const all = await deps.servers.list();
  return ok(all.map((l) => toServerView(l.server, l.toolCount)));
}

/** The decrypted server secret, for the gateway when it builds the upstream header. Null when unset. */
export async function getServerSecret(
  deps: Pick<CatalogDeps, 'servers' | 'secretBox'>,
  slug: string,
): Promise<string | null> {
  const server = await deps.servers.findBySlug(slug);
  const sealed = server?.sealedSecret ?? null;
  return sealed ? deps.secretBox.open(sealed) : null;
}

/** Call parameters of an active server; null when unknown or disabled. */
export async function getServerForUpstream(
  deps: Pick<CatalogDeps, 'servers'>,
  slug: string,
): Promise<ServerCallTarget | null> {
  const server = await deps.servers.findBySlug(slug);
  if (server?.status !== 'active') return null;
  return toServerCallTarget(server);
}
