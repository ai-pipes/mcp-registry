import type { Sql } from '../../../platform/db.ts';
import type { CallLog, CallLogEntry, CallLogFilter } from '../application/ports.ts';
import { CallOutcome } from '../domain/call-outcome.ts';

interface CallRow {
  id: string;
  user_id: string;
  token_id: string | null;
  server: string;
  tool: string;
  started_at: Date;
  duration_ms: number;
  outcome: string;
}

function toEntry(r: CallRow): CallLogEntry & { id: string } {
  if (!CallOutcome.is(r.outcome)) throw new Error(`unknown call outcome in gateway.calls: ${r.id}`);
  return {
    id: r.id,
    userId: r.user_id,
    tokenId: r.token_id,
    server: r.server,
    tool: r.tool,
    startedAt: r.started_at,
    durationMs: r.duration_ms,
    outcome: r.outcome,
  };
}

/**
 * The paging cursor: the pair (start, id) when both are given, else the start alone (strictly
 * older). `calls_started_at_idx` serves the range on the start time; ids only break ties.
 */
function cursor(sql: Sql, f: CallLogFilter) {
  if (f.before === undefined) return sql``;
  if (f.beforeId === undefined) return sql`and started_at < ${f.before}`;
  // `started_at <= before` is implied by the pair; stated alone it is what the index can range on.
  return sql`and started_at <= ${f.before}
    and (started_at, gateway.calls.id) < (${f.before}, ${f.beforeId}::bigint)`;
}

/** `gateway.calls`. Arguments and results have no column: they cannot end up here. */
export class PgCallLog implements CallLog {
  constructor(private readonly sql: Sql) {}

  async record(e: CallLogEntry): Promise<void> {
    await this.sql`insert into gateway.calls
        (user_id, token_id, server, tool, started_at, duration_ms, outcome)
      values (${e.userId}, ${e.tokenId}, ${e.server}, ${e.tool}, ${e.startedAt},
        ${Math.max(0, Math.round(e.durationMs))}, ${e.outcome})`;
  }

  async list(f: CallLogFilter): Promise<(CallLogEntry & { id: string })[]> {
    const sql = this.sql;
    const rows = await sql<CallRow[]>`select id::text as id, user_id, token_id, server, tool,
        started_at, duration_ms, outcome
      from gateway.calls
      where true
        ${f.userId === undefined ? sql`` : sql`and user_id = ${f.userId}`}
        ${f.server === undefined ? sql`` : sql`and server = ${f.server}`}
        ${f.outcome === undefined ? sql`` : sql`and outcome = ${f.outcome}`}
        ${cursor(sql, f)}
      order by started_at desc, gateway.calls.id desc
      limit ${f.limit}`;
    return rows.map(toEntry);
  }

  async deleteOlderThan(d: Date): Promise<number> {
    const r = await this.sql`delete from gateway.calls where started_at < ${d}`;
    return r.count;
  }
}
