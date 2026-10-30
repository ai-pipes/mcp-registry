import { CallEntry, CallList, CallsQuery } from '@mcp/contracts';
import { Hono } from 'hono';
import { readQuery } from '../../../../platform/http/request.ts';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireAdminRole,
  requireSession,
} from '../../../identity/index.ts';
import { listCalls } from '../../application/list-calls.ts';
import type { GatewayDeps } from '../../application/ports.ts';

/** `GET /api/calls`: the call log for admins. */
export function callRoutes(deps: Pick<GatewayDeps, 'callLog'>, identity: IdentityDeps) {
  return new Hono<IdentityEnv>()
    .use(requireSession(identity))
    .use(requireAdminRole)
    .get('/', async (c) => {
      const q = readQuery(c, CallsQuery);
      const r = await listCalls(deps, {
        principal: c.get('principal'),
        ...(q.user === undefined ? {} : { userId: q.user }),
        ...(q.server === undefined ? {} : { server: q.server }),
        ...(q.outcome === undefined ? {} : { outcome: q.outcome }),
        ...(q.before === undefined ? {} : { before: new Date(q.before) }),
        limit: q.limit,
      });
      if (!r.ok) throw r.error;
      const dtos = r.value.map((e) =>
        CallEntry.parse(Object.assign({}, e, { startedAt: e.startedAt.toISOString() })),
      );
      return c.json(CallList.parse(dtos));
    });
}
