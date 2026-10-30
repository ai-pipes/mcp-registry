import { AdminToolList, CatalogResponse, SetHiddenRequest } from '@mcp/contracts';
import { Hono } from 'hono';
import { readJson } from '../../../../platform/http/request.ts';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireAdminRole,
  requireSession,
} from '../../../identity/index.ts';
import { setToolHidden } from '../../application/commands/tools.ts';
import type { CatalogDeps } from '../../application/deps.ts';
import { listCatalogFor } from '../../application/queries/catalog.ts';
import { listServerTools } from '../../application/queries/tools.ts';
import { ToolRef } from '../../domain/tool-ref.ts';

/** `GET /api/catalog` (everyone) and a server's tools (admin). Mounted at `/api`. */
export function toolRoutes(deps: CatalogDeps, identity: IdentityDeps) {
  const session = requireSession(identity);
  return new Hono<IdentityEnv>()
    .get('/catalog', session, async (c) => {
      const entries = await listCatalogFor(deps, { principal: c.get('principal') });
      return c.json(CatalogResponse.parse(entries));
    })
    .get('/servers/:slug/tools', session, requireAdminRole, async (c) => {
      const r = await listServerTools(deps, {
        principal: c.get('principal'),
        slug: c.req.param('slug'),
      });
      if (!r.ok) throw r.error;
      return c.json(AdminToolList.parse(r.value));
    })
    .put('/servers/:slug/tools/:name/hidden', session, requireAdminRole, async (c) => {
      const { hidden } = await readJson(c, SetHiddenRequest);
      const r = await setToolHidden(deps, {
        principal: c.get('principal'),
        ref: ToolRef.format(c.req.param('slug'), c.req.param('name')),
        hidden,
      });
      if (!r.ok) throw r.error;
      return c.body(null, 204);
    });
}
