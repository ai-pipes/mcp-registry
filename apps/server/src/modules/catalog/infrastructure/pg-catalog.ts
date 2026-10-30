import postgres from 'postgres';
import type { Sql } from '../../../platform/db.ts';
import { appendOutbox } from '../../../platform/outbox.ts';
import type {
  ServerListing,
  ServerRepository,
  SnapshotResult,
  ToolRepository,
} from '../application/ports.ts';
import type { Access, ServerStatus } from '../domain/access.ts';
import type { AuthMode } from '../domain/auth-mode.ts';
import { CatalogError } from '../domain/errors.ts';
import { Server } from '../domain/server.ts';
import type { SyncRecord } from '../domain/sync.ts';
import { reconcileTools, Tool, type ToolAnnotations, type ToolSnapshot } from '../domain/tool.ts';

const UNIQUE_VIOLATION = '23505';

interface ServerRow {
  slug: string;
  name: string;
  description: string;
  url: string;
  auth: AuthMode;
  secret: Buffer | null;
  version: number;
  access: Access;
  timeout_ms: number;
  daily_limit: number | null;
  status: ServerStatus;
  last_sync_at: Date | null;
  last_sync_error: string | null;
  sync_user_id: string | null;
  created_at: Date;
  updated_at: Date;
}

interface ToolRow {
  server: string;
  name: string;
  title: string | null;
  description: string;
  input_schema: Record<string, unknown>;
  output_schema: Record<string, unknown> | null;
  annotations: ToolAnnotations;
  hidden: boolean;
  removed_at: Date | null;
  synced_at: Date;
}

const SERVER_COLUMNS = `slug, name, description, url, auth, secret, version, access, timeout_ms,
  daily_limit, status, last_sync_at, last_sync_error, sync_user_id, created_at, updated_at`;

const json = (value: unknown): string => JSON.stringify(value);

function hydrateServer(r: ServerRow): Server {
  return Server.rehydrate({
    slug: r.slug,
    name: r.name,
    description: r.description,
    url: r.url,
    auth: r.auth,
    sealedSecret: r.secret ? new Uint8Array(r.secret) : null,
    version: r.version,
    access: r.access,
    timeoutMs: r.timeout_ms,
    dailyLimit: r.daily_limit,
    status: r.status,
    lastSyncAt: r.last_sync_at,
    lastSyncError: r.last_sync_error,
    syncUserId: r.sync_user_id,
    createdAt: r.created_at,
    updatedAt: r.updated_at,
  });
}

function hydrateTool(r: ToolRow): Tool {
  return Tool.rehydrate({
    server: r.server,
    name: r.name,
    title: r.title,
    description: r.description,
    inputSchema: r.input_schema,
    outputSchema: r.output_schema,
    annotations: r.annotations,
    hidden: r.hidden,
    removedAt: r.removed_at,
    syncedAt: r.synced_at,
  });
}

export class PgServerRepository implements ServerRepository {
  constructor(private readonly sql: Sql) {}

  async findBySlug(slug: string): Promise<Server | null> {
    const [row] = await this.sql<ServerRow[]>`
      select ${this.sql.unsafe(SERVER_COLUMNS)} from catalog.servers where slug = ${slug}`;
    return row ? hydrateServer(row) : null;
  }

  async list(): Promise<ServerListing[]> {
    const rows = await this.sql<(ServerRow & { tool_count: number })[]>`
      select ${this.sql.unsafe(SERVER_COLUMNS.replace(/\b(\w+)\b/g, 's.$1'))},
        count(t.name)::int as tool_count
      from catalog.servers s
      left join catalog.tools t on t.server = s.slug and not t.hidden and t.removed_at is null
      group by s.slug
      order by s.slug`;
    return rows.map((r) => ({ server: hydrateServer(r), toolCount: r.tool_count }));
  }

  async add(server: Server): Promise<void> {
    const events = server.pullEvents();
    try {
      await this.sql.begin(async (tx) => {
        await tx`insert into catalog.servers (slug, name, description, url, auth, secret, access,
            timeout_ms, daily_limit, status, last_sync_at, last_sync_error, sync_user_id,
            created_at, updated_at)
          values (${server.slug}, ${server.name}, ${server.description}, ${server.url},
            ${json(server.auth)}::text::jsonb, ${server.sealedSecret ? Buffer.from(server.sealedSecret) : null},
            ${json(server.access)}::text::jsonb, ${server.timeoutMs}, ${server.dailyLimit},
            ${server.status}, ${server.lastSyncAt}, ${server.lastSyncError}, ${server.syncUserId},
            ${server.createdAt}, ${server.updatedAt})`;
        await appendOutbox(tx, events);
      });
    } catch (cause) {
      if (cause instanceof postgres.PostgresError && cause.code === UNIQUE_VIOLATION) {
        throw new CatalogError('server_exists', `server "${server.slug}" already exists`);
      }
      throw cause;
    }
  }

  /**
   * Optimistic concurrency: the row is written only if it is still at the version the aggregate
   * was read at (and the version is bumped); otherwise `server_conflict` and nothing is written,
   * so an edit based on a stale read never undoes or mixes with another admin's change (a secret
   * entered for the old URL is not bound to a new one). Synchronisation state (last_sync_*,
   * sync_user_id) is not the aggregate's to change here. The secret is written only when the
   * aggregate changed it (`secretChanged`): a key-rotation re-seal (compare-and-set, no version
   * bump) since the read is kept.
   */
  async save(server: Server): Promise<void> {
    const events = server.pullEvents();
    await this.sql.begin(async (tx) => {
      const written = await tx`update catalog.servers set
          name = ${server.name}, description = ${server.description}, url = ${server.url},
          auth = ${json(server.auth)}::text::jsonb,
          access = ${json(server.access)}::text::jsonb, timeout_ms = ${server.timeoutMs},
          daily_limit = ${server.dailyLimit}, status = ${server.status},
          updated_at = ${server.updatedAt}, version = version + 1
        where slug = ${server.slug} and version = ${server.version}`;
      if (written.count !== 1) {
        const [exists] = await tx`select 1 from catalog.servers where slug = ${server.slug}`;
        if (!exists) throw new CatalogError('not_found', `server "${server.slug}" not found`);
        throw new CatalogError(
          'server_conflict',
          `server "${server.slug}" was changed meanwhile; reload it and try again`,
        );
      }
      if (server.secretChanged) {
        const sealed = server.sealedSecret ? Buffer.from(server.sealedSecret) : null;
        await tx`update catalog.servers set secret = ${sealed} where slug = ${server.slug}`;
      }
      await appendOutbox(tx, events);
    });
  }

  async saveSyncState(slug: string, record: SyncRecord): Promise<void> {
    // Same rule as Server.applySync. The guard sits in the UPDATE's own WHERE, so Postgres
    // re-checks it on the locked row: overlapping runs cannot slip past each other.
    const written = await this.sql`update catalog.servers set
        last_sync_at = ${record.startedAt}, last_sync_error = ${record.error},
        sync_user_id = coalesce(${record.syncedBy}::uuid, sync_user_id)
      where slug = ${slug}
        and (last_sync_at is null or last_sync_at <= ${record.startedAt})`;
    if (written.count === 1) return;
    const [exists] = await this.sql`select 1 from catalog.servers where slug = ${slug}`;
    if (!exists) throw new CatalogError('not_found', `server "${slug}" not found`);
  }

  async replaceSealedSecret(slug: string, current: Uint8Array, next: Uint8Array): Promise<boolean> {
    const written = await this.sql`update catalog.servers set secret = ${Buffer.from(next)}
      where slug = ${slug} and secret = ${Buffer.from(current)}`;
    return written.count === 1;
  }
}

const TOOL_COLUMNS = `server, name, title, description, input_schema, output_schema, annotations,
  hidden, removed_at, synced_at`;

export class PgToolRepository implements ToolRepository {
  constructor(private readonly sql: Sql) {}

  async find(server: string, name: string): Promise<Tool | null> {
    const [row] = await this.sql<ToolRow[]>`
      select ${this.sql.unsafe(TOOL_COLUMNS)} from catalog.tools
      where server = ${server} and name = ${name}`;
    return row ? hydrateTool(row) : null;
  }

  async listByServer(server: string): Promise<Tool[]> {
    const rows = await this.sql<ToolRow[]>`
      select ${this.sql.unsafe(TOOL_COLUMNS)} from catalog.tools
      where server = ${server} order by name`;
    return rows.map(hydrateTool);
  }

  async save(tool: Tool): Promise<void> {
    const events = tool.pullEvents();
    await this.sql.begin(async (tx) => {
      const written = await tx`update catalog.tools set hidden = ${tool.hidden}
        where server = ${tool.server} and name = ${tool.name}`;
      if (written.count !== 1) throw new CatalogError('not_found', `tool "${tool.ref}" not found`);
      await appendOutbox(tx, events);
    });
  }

  async applySnapshot(
    server: string,
    snapshot: readonly ToolSnapshot[],
    now: Date,
  ): Promise<SnapshotResult> {
    return this.sql.begin(async (tx) => {
      // Serialises concurrent synchronisations of one server and fails early for an unknown one.
      const [locked] = await tx`select slug from catalog.servers where slug = ${server} for update`;
      if (!locked) throw new CatalogError('not_found', `server "${server}" not found`);
      const rows = await tx<ToolRow[]>`
        select ${this.sql.unsafe(TOOL_COLUMNS)} from catalog.tools where server = ${server}`;
      const r = reconcileTools(server, rows.map(hydrateTool), snapshot, now);
      if (r.upserts.length > 0) {
        const batch = json(
          r.upserts.map((t) => ({
            server: t.server,
            name: t.name,
            title: t.title,
            description: t.description,
            input_schema: t.inputSchema,
            output_schema: t.outputSchema,
            annotations: t.annotations,
            hidden: t.hidden,
            removed_at: t.removedAt,
            synced_at: t.syncedAt,
          })),
        );
        await tx`insert into catalog.tools (server, name, title, description, input_schema,
            output_schema, annotations, hidden, removed_at, synced_at)
          select server, name, title, description, input_schema, output_schema, annotations,
            hidden, removed_at, synced_at
          from jsonb_to_recordset(${batch}::text::jsonb) as x(
            server text, name text, title text, description text, input_schema jsonb,
            output_schema jsonb, annotations jsonb, hidden boolean, removed_at timestamptz,
            synced_at timestamptz)
          on conflict (server, name) do update set
            title = excluded.title, description = excluded.description,
            input_schema = excluded.input_schema, output_schema = excluded.output_schema,
            annotations = excluded.annotations, removed_at = excluded.removed_at,
            synced_at = excluded.synced_at`;
      }
      await appendOutbox(tx, r.events);
      return { changedRefs: r.changedRefs, addedRefs: r.addedRefs, removedRefs: r.removedRefs };
    });
  }
}
