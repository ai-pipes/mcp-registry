import { err, ok, type Result } from '../../../../shared-kernel/index.ts';
import type { Access, ServerStatus } from '../../domain/access.ts';
import type { AuthMode } from '../../domain/auth-mode.ts';
import { CatalogError } from '../../domain/errors.ts';
import { Server } from '../../domain/server.ts';
import { requireAdmin } from '../admin.ts';
import type { CatalogDeps } from '../deps.ts';
import type { Principal, ServerView } from '../ports.ts';
import { toServerView } from '../views.ts';

type Editable = {
  name: string;
  description: string;
  url: string;
  auth: AuthMode;
  access: Access;
  timeoutMs: number;
  dailyLimit: number | null;
};

const notFound = (slug: string) => err(new CatalogError('not_found', `server "${slug}" not found`));

async function load(deps: CatalogDeps, slug: string): Promise<Result<Server, CatalogError>> {
  const server = await deps.servers.findBySlug(slug);
  return server ? ok(server) : notFound(slug);
}

/** Runs a repository write; CatalogErrors (server_exists…) become Results. */
async function write(run: () => Promise<void>): Promise<Result<void, CatalogError>> {
  try {
    await run();
    return ok(undefined);
  } catch (cause) {
    if (cause instanceof CatalogError) return err(cause);
    throw cause;
  }
}

export async function createServer(
  deps: CatalogDeps,
  cmd: {
    principal: Principal;
    slug: string;
    name: string;
    description: string;
    url: string;
    auth: AuthMode;
    access: Access;
    timeoutMs?: number;
    dailyLimit?: number | null;
    secret?: string | null;
  },
): Promise<Result<ServerView, CatalogError>> {
  const admin = requireAdmin(cmd.principal);
  if (!admin.ok) return admin;
  const now = deps.clock.now();
  const created = Server.create({ ...cmd, now });
  if (!created.ok) return created;
  const server = created.value;
  if (cmd.secret !== undefined && cmd.secret !== null) {
    const secret = Server.checkSecret(cmd.secret);
    if (!secret.ok) return secret;
    const stored = server.setSealedSecret(deps.secretBox.seal(secret.value), now);
    if (!stored.ok) return stored;
  }
  const saved = await write(() => deps.servers.add(server));
  if (!saved.ok) return saved;
  const actorId = cmd.principal.userId;
  await deps.audit.record({
    actorId,
    action: 'server.create',
    target: server.slug,
    meta: { auth: server.auth.kind },
  });
  if (server.secretSet) {
    await deps.audit.record({ actorId, action: 'server.secret_set', target: server.slug });
  }
  return ok(toServerView(server, 0));
}

export async function updateServer(
  deps: CatalogDeps,
  cmd: { principal: Principal; slug: string; changes: Partial<Editable> },
): Promise<Result<ServerView, CatalogError>> {
  const admin = requireAdmin(cmd.principal);
  if (!admin.ok) return admin;
  const loaded = await load(deps, cmd.slug);
  if (!loaded.ok) return loaded;
  const server = loaded.value;
  const changed = server.update(cmd.changes, deps.clock.now());
  if (!changed.ok) return changed;
  await deps.servers.save(server);
  await deps.audit.record({
    actorId: cmd.principal.userId,
    action: 'server.update',
    target: server.slug,
    meta: { fields: Object.keys(cmd.changes) },
  });
  if (changed.value.secretCleared) {
    await deps.audit.record({
      actorId: cmd.principal.userId,
      action: 'server.secret_clear',
      target: server.slug,
    });
  }
  const listing = (await deps.servers.list()).find((l) => l.server.slug === server.slug);
  return ok(toServerView(server, listing?.toolCount ?? 0));
}

/** `secret: null` clears it. The value never reaches the audit log. */
export async function setServerSecret(
  deps: CatalogDeps,
  cmd: { principal: Principal; slug: string; secret: string | null },
): Promise<Result<void, CatalogError>> {
  const admin = requireAdmin(cmd.principal);
  if (!admin.ok) return admin;
  const loaded = await load(deps, cmd.slug);
  if (!loaded.ok) return loaded;
  const server = loaded.value;
  let sealed: Uint8Array | null = null;
  if (cmd.secret !== null) {
    const secret = Server.checkSecret(cmd.secret);
    if (!secret.ok) return secret;
    sealed = deps.secretBox.seal(secret.value);
  }
  const stored = server.setSealedSecret(sealed, deps.clock.now());
  if (!stored.ok) return stored;
  await deps.servers.save(server);
  await deps.audit.record({
    actorId: cmd.principal.userId,
    action: sealed ? 'server.secret_set' : 'server.secret_clear',
    target: server.slug,
  });
  return ok(undefined);
}

export async function setServerStatus(
  deps: CatalogDeps,
  cmd: { principal: Principal; slug: string; status: ServerStatus },
): Promise<Result<void, CatalogError>> {
  const admin = requireAdmin(cmd.principal);
  if (!admin.ok) return admin;
  const loaded = await load(deps, cmd.slug);
  if (!loaded.ok) return loaded;
  const server = loaded.value;
  if (server.setStatus(cmd.status, deps.clock.now())) {
    await deps.servers.save(server);
    await deps.audit.record({
      actorId: cmd.principal.userId,
      action: 'server.status',
      target: server.slug,
      meta: { status: cmd.status },
    });
  }
  return ok(undefined);
}
