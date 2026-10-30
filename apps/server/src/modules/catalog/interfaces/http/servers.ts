import {
  type AuthInput,
  CreateServerRequest,
  ServerList,
  ServerView,
  SetSecretRequest,
  SetStatusRequest,
  type SyncResult,
  UpdateServerRequest,
} from '@mcp/contracts';
import { Hono } from 'hono';
import { readJson } from '../../../../platform/http/request.ts';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireAdminRole,
  requireSession,
} from '../../../identity/index.ts';
import { registerServer } from '../../application/commands/register.ts';
import {
  setServerSecret,
  setServerStatus,
  updateServer,
} from '../../application/commands/servers.ts';
import { syncServer } from '../../application/commands/sync.ts';
import type { SyncDeps } from '../../application/deps.ts';
import type { ServerView as ServerViewModel } from '../../application/ports.ts';
import { listAllServers } from '../../application/queries/servers.ts';
import type { AuthMode } from '../../domain/auth-mode.ts';
import { CatalogError } from '../../domain/errors.ts';

/**
 * The response shape of a server. Parsing through the contract drops everything it does not
 * name, so a field added to the module's view cannot reach the wire unnoticed.
 */
function toDto(v: ServerViewModel): ServerView {
  return ServerView.parse({
    ...v,
    lastSyncAt: v.lastSyncAt?.toISOString() ?? null,
  });
}

/** The request's auth settings as the domain's `AuthMode` (absent optional fields stay absent). */
export function toAuthMode(a: AuthInput): AuthMode {
  if (a.kind !== 'oauth_user') return a;
  const { revokeUrl, extraAuthorizeParams, ...rest } = a.oauth;
  return {
    ...a,
    oauth: {
      ...rest,
      ...(revokeUrl === undefined ? {} : { revokeUrl }),
      ...(extraAuthorizeParams === undefined ? {} : { extraAuthorizeParams }),
    },
  };
}

/** `/api/servers`: administration of upstream servers. Admins only. */
export function serverRoutes(deps: SyncDeps, identity: IdentityDeps) {
  const all = async (principal: IdentityEnv['Variables']['principal']) => {
    const r = await listAllServers(deps, { principal });
    if (!r.ok) throw r.error;
    return r.value;
  };
  return new Hono<IdentityEnv>()
    .use(requireSession(identity))
    .use(requireAdminRole)
    .get('/', async (c) => c.json(ServerList.parse((await all(c.get('principal'))).map(toDto))))
    .post('/', async (c) => {
      const { secret, auth, ...input } = await readJson(c, CreateServerRequest);
      const r = await registerServer(deps, {
        principal: c.get('principal'),
        ...input,
        auth: toAuthMode(auth),
        ...(secret === undefined ? {} : { secret }),
      });
      if (!r.ok) throw r.error;
      return c.json(toDto(r.value), 201);
    })
    .get('/:slug', async (c) => {
      const found = (await all(c.get('principal'))).find((s) => s.slug === c.req.param('slug'));
      if (!found) throw new CatalogError('not_found', 'server not found');
      return c.json(toDto(found));
    })
    .put('/:slug', async (c) => {
      const { auth, ...rest } = await readJson(c, UpdateServerRequest);
      const r = await updateServer(deps, {
        principal: c.get('principal'),
        slug: c.req.param('slug'),
        changes: { ...rest, auth: toAuthMode(auth) },
      });
      if (!r.ok) throw r.error;
      return c.json(toDto(r.value));
    })
    .put('/:slug/secret', async (c) => {
      const { secret } = await readJson(c, SetSecretRequest);
      const r = await setServerSecret(deps, {
        principal: c.get('principal'),
        slug: c.req.param('slug'),
        secret,
      });
      if (!r.ok) throw r.error;
      return c.body(null, 204);
    })
    .post('/:slug/status', async (c) => {
      const { status } = await readJson(c, SetStatusRequest);
      const r = await setServerStatus(deps, {
        principal: c.get('principal'),
        slug: c.req.param('slug'),
        status,
      });
      if (!r.ok) throw r.error;
      return c.body(null, 204);
    })
    .post('/:slug/sync', async (c) => {
      const r = await syncServer(deps, {
        principal: c.get('principal'),
        slug: c.req.param('slug'),
      });
      if (!r.ok) throw r.error;
      const body: SyncResult = r.value;
      return c.json(body);
    });
}
