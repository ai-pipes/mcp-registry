import type { Sql } from '../../../platform/db.ts';
import type { Quota } from '../application/ports.ts';
import { QuotaDay } from '../domain/quota-day.ts';

/**
 * `gateway.quota`: one atomic upsert per call. The `where count < limit` on the conflict branch
 * makes the row lock decide: concurrent calls of one person to one server never pass the limit.
 */
export class PgQuota implements Quota {
  constructor(private readonly sql: Sql) {}

  async tryConsume(i: {
    userId: string;
    server: string;
    day: string;
    limit: number;
  }): Promise<{ ok: true } | { ok: false; resetAt: Date }> {
    const rows = await this.sql`insert into gateway.quota (user_id, server, day, count)
      values (${i.userId}, ${i.server}, ${i.day}, 1)
      on conflict (user_id, server, day) do update set count = gateway.quota.count + 1
        where gateway.quota.count < ${i.limit}
      returning count`;
    return rows.length > 0 ? { ok: true } : { ok: false, resetAt: QuotaDay.resetAt(i.day) };
  }

  async deleteDaysBefore(day: string): Promise<number> {
    const r = await this.sql`delete from gateway.quota where day < ${day}`;
    return r.count;
  }
}
