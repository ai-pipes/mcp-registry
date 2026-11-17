import { cp, mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vite-plus/test';
import { migrate, NewerDatabaseError } from '../../src/platform/migrate.ts';
import { migrationsDir, startPg } from '../pg.ts';

let pg: Awaited<ReturnType<typeof startPg>>;
beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});

// A copy of the bundled migrations plus extra files, so the database knows no foreign names.
async function dirWith(extra: Record<string, string>): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'mig-'));
  await cp(migrationsDir, dir, { recursive: true });
  await Promise.all(Object.entries(extra).map(([name, text]) => writeFile(join(dir, name), text)));
  return dir;
}

describe('migrate', () => {
  it('applies migrations once', async () => {
    expect(await migrate(pg.sql, migrationsDir)).toEqual([]);
    const rows = await pg.sql`select name from platform.schema_migrations`;
    expect(rows.map((r) => r.name)).toContain('0001_platform.sql');
  });

  it('creates the platform schema: outbox, audit_log and key_check', async () => {
    const rows = await pg.sql<{ table_name: string }[]>`
      select table_name from information_schema.tables where table_schema = 'platform'`;
    expect(rows.map((r) => r.table_name).toSorted()).toEqual([
      'audit_log',
      'key_check',
      'outbox',
      'schema_migrations',
    ]);
  });

  it('rolls back a failing migration and does not record it', async () => {
    const dir = await dirWith({
      '0099_bad.sql': 'create table platform.tmp_t (id int); select 1/0;',
    });
    await expect(migrate(pg.sql, dir)).rejects.toThrow('division by zero');
    expect(await pg.sql`select to_regclass('platform.tmp_t') as t`).toEqual([{ t: null }]);
    const names = (await pg.sql`select name from platform.schema_migrations`).map((r) => r.name);
    expect(names).not.toContain('0099_bad.sql');
  });

  it('applies a new migration on top of the known ones', async () => {
    const dir = await dirWith({ '0098_ok.sql': 'create table platform.tmp_ok (id int);' });
    expect(await migrate(pg.sql, dir)).toEqual(['0098_ok.sql']);
    await pg.sql`drop table platform.tmp_ok`;
    await pg.sql`delete from platform.schema_migrations where name = '0098_ok.sql'`;
  });

  it('refuses a database migrated by a newer version and applies nothing', async () => {
    const dir = await dirWith({
      '0097_pending.sql': 'create table platform.tmp_pending (id int);',
    });
    await pg.sql`insert into platform.schema_migrations (name) values ('9999_from_the_future.sql')`;
    try {
      const error = await migrate(pg.sql, dir).catch((e: unknown) => e);
      expect(error).toBeInstanceOf(NewerDatabaseError);
      expect(error).toMatchObject({ migration: '9999_from_the_future.sql' });
      expect(await pg.sql`select to_regclass('platform.tmp_pending') as t`).toEqual([{ t: null }]);
      const names = (await pg.sql`select name from platform.schema_migrations`).map((r) => r.name);
      expect(names).not.toContain('0097_pending.sql');
    } finally {
      await pg.sql`delete from platform.schema_migrations where name = '9999_from_the_future.sql'`;
    }
    // The advisory lock was released: a normal run works again.
    expect(await migrate(pg.sql, migrationsDir)).toEqual([]);
  });
});
