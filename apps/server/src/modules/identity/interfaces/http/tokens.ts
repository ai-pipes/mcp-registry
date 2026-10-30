import { CreateTokenRequest } from '@mcp/contracts';
import { Hono } from 'hono';
import { readJson } from '../../../../platform/http/request.ts';
import type { IdentityDeps } from '../../application/ports.ts';
import { createToken, listTokens, revokeToken } from '../../application/tokens.ts';
import { requireSession } from './session.ts';
import { type IdentityEnv, parseId, unwrap } from './shared.ts';

/** `/api/tokens`: the caller's own API tokens. */
export function tokenRoutes(deps: IdentityDeps) {
  return new Hono<IdentityEnv>()
    .use(requireSession(deps))
    .get('/', async (c) => c.json(await listTokens(deps, { principal: c.get('principal') })))
    .post('/', async (c) => {
      const { name } = await readJson(c, CreateTokenRequest);
      const t = await createToken(deps, { principal: c.get('principal'), name });
      return c.json({ id: t.id, prefix: t.prefix, token: t.token }, 201);
    })
    .delete('/:id', async (c) => {
      const id = parseId(c.req.param('id'), 'token');
      unwrap(await revokeToken(deps, { principal: c.get('principal'), id }));
      return c.body(null, 204);
    });
}
