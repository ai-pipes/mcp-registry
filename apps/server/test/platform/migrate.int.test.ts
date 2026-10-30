import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vite-plus/test';
import { migrate } from '../../src/platform/migrate.ts';
import { migrationsDir, startPg } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});

describe('migrate', () => {
  it('applies migrations once', async () => {
    expect(await migrate(pg.sql, migrationsDir)).toEqual([]);
    const rows = await pg.sql`select name from platform.schema_migrations`;
    expect(rows.map((r) => r.name)).toContain('0001_platform.sql');
  });

  it('creates the platform schema: outbox and audit_log', async () => {
    const rows = await pg.sql<{ table_name: string }[]>`
      select table_name from information_schema.tables where table_schema = 'platform'`;
    expect(rows.map((r) => r.table_name).toSorted()).toEqual([
      'audit_log',
      'outbox',
      'schema_migrations',
    ]);
  });

  it('rolls back a failing migration and does not record it', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'mig-bad-'));
    await writeFile(join(dir, '0001_platform.sql'), 'select 1;');
    await writeFile(join(dir, '0002_bad.sql'), 'create table platform.tmp_t (id int); select 1/0;');
    await expect(migrate(pg.sql, dir)).rejects.toThrow();
    expect(await pg.sql`select to_regclass('platform.tmp_t') as t`).toEqual([{ t: null }]);
    const names = (await pg.sql`select name from platform.schema_migrations`).map((r) => r.name);
    expect(names).not.toContain('0002_bad.sql');
  });
});
