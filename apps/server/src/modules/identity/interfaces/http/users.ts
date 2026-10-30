import { SetDisabledRequest } from '@mcp/contracts';
import { Hono } from 'hono';
import { errorResponse } from '../../../../platform/http/errors.ts';
import { readJson } from '../../../../platform/http/request.ts';
import type { IdentityDeps } from '../../application/ports.ts';
import { listUsers, setDisabled } from '../../application/users.ts';
import { requireSession } from './session.ts';
import { type IdentityEnv, parseId, unwrap } from './shared.ts';

/** `/api/users`: admin only. The use cases enforce it too; the route check runs before parsing. */
export function userRoutes(deps: IdentityDeps) {
  return new Hono<IdentityEnv>()
    .use(requireSession(deps))
    .use((c, next) =>
      c.get('principal').role === 'admin'
        ? next()
        : Promise.resolve(errorResponse(c, 'forbidden', 'admin only')),
    )
    .get('/', async (c) => c.json(unwrap(await listUsers(deps, { principal: c.get('principal') }))))
    .post('/:id/disabled', async (c) => {
      const userId = parseId(c.req.param('id'), 'user');
      const { disabled } = await readJson(c, SetDisabledRequest);
      unwrap(await setDisabled(deps, { principal: c.get('principal'), userId, disabled }));
      return c.body(null, 204);
    });
}
