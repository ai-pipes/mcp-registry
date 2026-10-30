import { fileURLToPath } from 'node:url';
import { PostgreSqlContainer } from '@testcontainers/postgresql';
import { createDb, type Sql } from '../src/platform/db.ts';
import { migrate } from '../src/platform/migrate.ts';

export const migrationsDir = fileURLToPath(new URL('../migrations', import.meta.url));

export async function startPg(): Promise<{ sql: Sql; url: string; stop(): Promise<void> }> {
  const container = await new PostgreSqlContainer('pgvector/pgvector:pg17').start();
  const url = container.getConnectionUri();
  const sql = createDb(url);
  await migrate(sql, migrationsDir);
  return {
    sql,
    url,
    stop: async () => {
      await sql.end();
      await container.stop();
    },
  };
}

/** Empties every table of the platform schema and of the module schemas (not the migrations log). */
export async function truncateAll(sql: Sql): Promise<void> {
  const tables = await sql<{ s: string; t: string }[]>`
    select schemaname as s, tablename as t from pg_tables
    where schemaname in ('platform', 'identity', 'catalog', 'connections', 'search', 'gateway')
      and tablename <> 'schema_migrations'`;
  if (tables.length === 0) return;
  const list = tables.map((x) => `"${x.s}"."${x.t}"`).join(', ');
  await sql.unsafe(`truncate ${list} restart identity cascade`);
}
