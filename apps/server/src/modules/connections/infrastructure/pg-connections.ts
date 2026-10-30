import type { Sql, Tx } from '../../../platform/db.ts';
import type { ConnectionStore, PendingConnects } from '../application/ports.ts';
import { Connection, type ConnectionKind, type ConnectionStatus } from '../domain/connection.ts';
import type { PendingConnect } from '../domain/pending-connect.ts';

interface ConnectionRow {
  user_id: string;
  server: string;
  kind: ConnectionKind;
  access: Buffer;
  refresh: Buffer | null;
  expires_at: Date | null;
  status: ConnectionStatus;
  issued_for: string;
  revoke_url: string | null;
  updated_at: Date;
}

const toConnection = (r: ConnectionRow): Connection =>
  Connection.restore({
    userId: r.user_id,
    server: r.server,
    kind: r.kind,
    access: new Uint8Array(r.access),
    refresh: r.refresh === null ? null : new Uint8Array(r.refresh),
    expiresAt: r.expires_at,
    status: r.status,
    issuedFor: r.issued_for,
    revokeUrl: r.revoke_url,
    updatedAt: r.updated_at,
  });

async function upsert(tx: Sql | Tx, c: Connection): Promise<void> {
  const s = c.snapshot();
  await tx`insert into connections.connections
      (user_id, server, kind, access, refresh, expires_at, status, issued_for, revoke_url,
        updated_at)
    values (${s.userId}, ${s.server}, ${s.kind}, ${Buffer.from(s.access)},
      ${s.refresh === null ? null : Buffer.from(s.refresh)}, ${s.expiresAt}, ${s.status},
      ${s.issuedFor}, ${s.revokeUrl}, ${s.updatedAt})
    on conflict (user_id, server) do update set
      kind = excluded.kind, access = excluded.access, refresh = excluded.refresh,
      expires_at = excluded.expires_at, status = excluded.status,
      issued_for = excluded.issued_for, revoke_url = excluded.revoke_url,
      updated_at = excluded.updated_at`;
}

export class PgConnectionStore implements ConnectionStore {
  constructor(private readonly sql: Sql) {}

  async get(userId: string, server: string): Promise<Connection | null> {
    const [r] = await this.sql<ConnectionRow[]>`select * from connections.connections
      where user_id = ${userId} and server = ${server}`;
    return r ? toConnection(r) : null;
  }

  async listForUser(userId: string): Promise<Connection[]> {
    const rows = await this.sql<ConnectionRow[]>`select * from connections.connections
      where user_id = ${userId} order by server`;
    return rows.map(toConnection);
  }

  /**
   * `select … for update` in a transaction: a second process asking for the same row waits until
   * the first commits and then sees its result. A missing row locks nothing; inserts of the same
   * key still serialise on the primary key.
   */
  async withLock<T>(
    userId: string,
    server: string,
    fn: (current: Connection | null, save: (c: Connection) => Promise<void>) => Promise<T>,
  ): Promise<T> {
    const result = await this.sql.begin(async (tx) => {
      const [r] = await tx<ConnectionRow[]>`select * from connections.connections
        where user_id = ${userId} and server = ${server} for update`;
      return [await fn(r ? toConnection(r) : null, (c) => upsert(tx, c))] as const;
    });
    return result[0];
  }

  async delete(userId: string, server: string): Promise<Connection | null> {
    const [r] = await this.sql<ConnectionRow[]>`delete from connections.connections
      where user_id = ${userId} and server = ${server} returning *`;
    return r ? toConnection(r) : null;
  }

  async listKeys(): Promise<{ userId: string; server: string }[]> {
    const rows = await this.sql<{ user_id: string; server: string }[]>`
      select user_id, server from connections.connections order by user_id, server`;
    return rows.map((r) => ({ userId: r.user_id, server: r.server }));
  }
}

interface PendingRow {
  state: string;
  user_id: string;
  server: string;
  code_verifier: string;
  issued_for: string;
  expires_at: Date;
}

export class PgPendingConnects implements PendingConnects {
  constructor(private readonly sql: Sql) {}

  async insert(p: PendingConnect): Promise<void> {
    // Abandoned attempts would pile up otherwise; an hour of slack keeps clock skew harmless.
    await this.sql`delete from connections.pending where expires_at < now() - interval '1 hour'`;
    await this.sql`insert into connections.pending
        (state, user_id, server, code_verifier, issued_for, expires_at)
      values (${p.state}, ${p.userId}, ${p.server}, ${p.codeVerifier}, ${p.issuedFor},
        ${p.expiresAt})`;
  }

  async consume(state: string): Promise<PendingConnect | null> {
    const [r] = await this.sql<PendingRow[]>`
      delete from connections.pending where state = ${state} returning *`;
    return r
      ? {
          state: r.state,
          userId: r.user_id,
          server: r.server,
          codeVerifier: r.code_verifier,
          issuedFor: r.issued_for,
          expiresAt: r.expires_at,
        }
      : null;
  }
}
