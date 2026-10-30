import { AuditList, AuditQuery } from '@mcp/contracts';
import { Hono } from 'hono';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireAdminRole,
  requireSession,
} from '../../modules/identity/index.ts';
import type { Sql } from '../db.ts';
import { readQuery } from './request.ts';

type Row = {
  id: string;
  at: Date;
  actor_id: string | null;
  action: string;
  target: string | null;
  meta: Record<string, unknown> | null;
};

/** `GET /api/audit`: what admins did (platform's own table; meta carries no secret values). */
export function auditRoutes(sql: Sql, identity: IdentityDeps) {
  return new Hono<IdentityEnv>()
    .use(requireSession(identity))
    .use(requireAdminRole)
    .get('/', async (c) => {
      const q = readQuery(c, AuditQuery);
      const rows = await sql<Row[]>`select id::text as id, at, actor_id, action, target, meta
        from platform.audit_log
        where true ${q.before === undefined ? sql`` : sql`and id < ${q.before}::bigint`}
        order by id desc
        limit ${q.limit}`;
      return c.json(
        AuditList.parse(
          rows.map((r) => ({
            id: r.id,
            at: r.at.toISOString(),
            actorId: r.actor_id,
            action: r.action,
            target: r.target,
            meta: r.meta ?? {},
          })),
        ),
      );
    });
}
