import {
  err,
  errorFields,
  ok,
  type Result,
  UpstreamError,
  type UpstreamTarget,
} from '../../../../shared-kernel/index.ts';
import { AuthMode } from '../../domain/auth-mode.ts';
import { CatalogError } from '../../domain/errors.ts';
import type { Server } from '../../domain/server.ts';
import { type SyncFailure, syncFailureText, SyncOwner, SyncRecord } from '../../domain/sync.ts';
import { snapshotFromUpstream } from '../../domain/tool.ts';
import { requireAdmin } from '../admin.ts';
import type { SyncDeps } from '../deps.ts';
import type { Principal } from '../ports.ts';
import { toServerCallTarget } from '../views.ts';

export type SyncCounts = { added: number; changed: number; removed: number };
export type SyncOutcome = { slug: string; ok: true } | { slug: string; ok: false; code: string };

/** Upstream headers for synchronising this server, or why there are none. */
async function headersFor(
  deps: SyncDeps,
  server: Server,
  startedBy: string | null,
): Promise<Result<Record<string, string>, SyncFailure>> {
  const source = AuthMode.credentialSource(server.auth);
  if (source === 'none') return ok({});
  if (source === 'personal') {
    const owner = server.syncCredentialsOwner(startedBy);
    const active = owner !== null && (await deps.credentials.isActiveAdmin(owner));
    if (!SyncOwner.mayLend(owner, active)) return err('needs_connection');
    const secret = await deps.credentials.personalSecret(toServerCallTarget(server), owner);
    if (!secret.ok) return err(SyncOwner.failureOf(secret.error));
    return ok(AuthMode.headerFor(server.auth, secret.value));
  }
  const sealed = server.sealedSecret;
  if (!sealed) return err('no_secret');
  return ok(AuthMode.headerFor(server.auth, deps.secretBox.open(sealed)));
}

/**
 * Fetches upstream `tools/list` and stores it as the server's tool snapshot. `principal` is the
 * admin who pressed the button; `null` is the scheduler. On any failure the tools stay as they
 * were and `last_sync_error` says why.
 */
export async function syncServer(
  deps: SyncDeps,
  cmd: { principal: Principal | null; slug: string },
): Promise<Result<SyncCounts, CatalogError>> {
  // The attempt's place in time: a slower, older attempt never overwrites a newer one.
  const startedAt = deps.clock.now();
  if (cmd.principal) {
    const admin = requireAdmin(cmd.principal);
    if (!admin.ok) return admin;
  }
  const server = await deps.servers.findBySlug(cmd.slug);
  if (server?.status !== 'active') {
    return err(new CatalogError('not_found', `active server "${cmd.slug}" not found`));
  }
  const startedBy = cmd.principal?.userId ?? null;
  const audit = async (meta: Record<string, unknown>) => {
    if (startedBy === null) return;
    await deps.audit.record({
      actorId: startedBy,
      action: 'server.sync',
      target: server.slug,
      meta,
    });
  };
  const fail = async (failure: SyncFailure): Promise<Result<SyncCounts, CatalogError>> => {
    await deps.servers.saveSyncState(server.slug, SyncRecord.failure(failure, startedAt));
    await audit({ error: failure });
    const code = failure === 'needs_connection' ? 'needs_connection' : 'sync_failed';
    return err(new CatalogError(code, syncFailureText(failure)));
  };

  const headers = await headersFor(deps, server, startedBy);
  if (!headers.ok) return fail(headers.error);
  const target: UpstreamTarget = {
    url: server.url,
    headers: headers.value,
    timeoutMs: server.timeoutMs,
    maxResponseBytes: deps.maxResponseBytes,
  };
  let listed;
  try {
    listed = await deps.upstream.listTools(target);
  } catch (error) {
    if (error instanceof UpstreamError) return fail(error.kind);
    throw error;
  }

  const now = deps.clock.now();
  const r = await deps.tools.applySnapshot(server.slug, listed.map(snapshotFromUpstream), now);
  await deps.servers.saveSyncState(server.slug, SyncRecord.success(startedBy, startedAt));
  const counts: SyncCounts = {
    added: r.addedRefs.length,
    changed: r.changedRefs.length - r.addedRefs.length,
    removed: r.removedRefs.length,
  };
  await audit(counts);
  return ok(counts);
}

/**
 * `syncServer` that absorbs an unexpected error (not an upstream failure: a database blip, a
 * secret no key opens): it is logged without its message and recorded as the server's `internal`
 * sync failure. Returns null in that case.
 */
export async function syncServerRecordingFailure(
  deps: SyncDeps,
  cmd: { principal: Principal | null; slug: string },
): Promise<Result<SyncCounts, CatalogError> | null> {
  const startedAt = deps.clock.now();
  try {
    return await syncServer(deps, cmd);
  } catch (error) {
    deps.logger?.error({ server: cmd.slug, err: errorFields(error) }, 'catalog sync failed');
    try {
      await deps.servers.saveSyncState(cmd.slug, SyncRecord.failure('internal', startedAt));
    } catch (stateError) {
      deps.logger?.error(
        { server: cmd.slug, err: errorFields(stateError) },
        'sync state not saved',
      );
    }
    return null;
  }
}

/**
 * The scheduler's run: every active server in turn; one failure does not stop the others, not
 * even an unexpected one (recorded as `internal`).
 */
export async function syncAllServers(deps: SyncDeps): Promise<SyncOutcome[]> {
  const listings = await deps.servers.list();
  const active = listings.filter((l) => l.server.status === 'active').map((l) => l.server.slug);
  // Sequential: a few upstreams, and no burst against them every interval.
  return active.reduce<Promise<SyncOutcome[]>>(async (prev, slug) => {
    const out = await prev;
    const r = await syncServerRecordingFailure(deps, { principal: null, slug });
    if (r === null) out.push({ slug, ok: false, code: 'internal' });
    else out.push(r.ok ? { slug, ok: true } : { slug, ok: false, code: r.error.code });
    return out;
  }, Promise.resolve([]));
}
