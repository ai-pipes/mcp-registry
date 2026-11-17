import { randomBytes } from 'node:crypto';
import { Writable } from 'node:stream';
import { z } from 'zod';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { checkSecretsKey } from '../../src/platform/bootstrap.ts';
import { resealKeyCheck, verifySecretsKey } from '../../src/platform/crypto/key-check.ts';
import { AesGcmSecretBox } from '../../src/platform/crypto/secret-box.ts';
import { createLogger } from '../../src/platform/logger.ts';
import { startPg, truncateAll } from '../pg.ts';

const LogRecord = z.record(z.string(), z.unknown());

let pg: Awaited<ReturnType<typeof startPg>>;
const key = () => new Uint8Array(randomBytes(32));

beforeAll(async () => {
  pg = await startPg();
});
afterAll(async () => {
  await pg.stop();
});
beforeEach(async () => {
  await truncateAll(pg.sql);
});

const rows = () => pg.sql<{ sealed: Uint8Array }[]>`select sealed from platform.key_check`;

describe('verifySecretsKey', () => {
  it('creates the check value on a database that has none', async () => {
    const current = key();
    expect(await verifySecretsKey(pg.sql, { current })).toBe('created');
    expect(await rows()).toHaveLength(1);
    expect(await verifySecretsKey(pg.sql, { current })).toBe('ok');
    expect(await rows()).toHaveLength(1);
  });

  it('keeps working data from before the check existed', async () => {
    const current = key();
    const sealed = new AesGcmSecretBox(current).seal('upstream-secret');
    await pg.sql`
      insert into catalog.servers
        (slug, name, url, auth, secret, access, timeout_ms, status, created_at, updated_at)
      values ('old', 'Old', 'https://up.example.com/mcp', '{"kind":"shared_secret"}'::jsonb,
              ${sealed}, '{"kind":"all"}'::jsonb, 30000, 'active', now(), now())`;
    expect(await verifySecretsKey(pg.sql, { current })).toBe('created');
  });

  it('two concurrent first checks both succeed and leave one row', async () => {
    const current = key();
    const results = await Promise.all([
      verifySecretsKey(pg.sql, { current }),
      verifySecretsKey(pg.sql, { current }),
    ]);
    for (const r of results) expect(['created', 'ok']).toContain(r);
    expect(await rows()).toHaveLength(1);
  });

  it('reports a mismatch for another key and leaves the row unchanged', async () => {
    await verifySecretsKey(pg.sql, { current: key() });
    const [before] = await rows();
    expect(await verifySecretsKey(pg.sql, { current: key() })).toBe('mismatch');
    const after = await rows();
    expect(after).toHaveLength(1);
    expect(Buffer.from(after[0]!.sealed).equals(Buffer.from(before!.sealed))).toBe(true);
  });

  it('reports needs_rotation while the old key is configured, and ok after resealKeyCheck', async () => {
    const previous = key();
    const current = key();
    await verifySecretsKey(pg.sql, { current: previous });
    expect(await verifySecretsKey(pg.sql, { current, previous })).toBe('needs_rotation');
    expect(await resealKeyCheck(pg.sql, { current, previous })).toBe('resealed');
    expect(await verifySecretsKey(pg.sql, { current })).toBe('ok');
    expect(await resealKeyCheck(pg.sql, { current, previous })).toBe('current');
  });
});

async function run(secretsKey: Uint8Array, secretsKeyOld?: Uint8Array) {
  const lines: string[] = [];
  const sink = new Writable({
    write(chunk: Buffer, _enc, done) {
      lines.push(chunk.toString());
      done();
    },
  });
  const config = { secretsKey, ...(secretsKeyOld ? { secretsKeyOld } : {}) };
  const result = await checkSecretsKey(pg.sql, config, createLogger('info', sink));
  return { result, records: lines.map((l) => LogRecord.parse(JSON.parse(l))) };
}

describe('checkSecretsKey', () => {
  it('logs one fatal record without key material on a mismatch and says to stop', async () => {
    const original = key();
    const wrong = key();
    await run(original);
    const { result, records } = await run(wrong);
    expect(result.proceed).toBe(false);
    expect(records).toHaveLength(1);
    expect(records[0]?.level).toBe(60);
    expect(records[0]?.msg).toBe(
      'SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one). See "Recovering from a key mismatch" in the README.',
    );
    const text = JSON.stringify(records);
    for (const k of [original, wrong]) {
      expect(text).not.toContain(Buffer.from(k).toString('base64'));
      expect(text).not.toContain(Buffer.from(k).toString('hex'));
    }
  });

  it('continues on first start (info) and when the key matches', async () => {
    const k = key();
    const first = await run(k);
    expect(first.result.proceed).toBe(true);
    expect(first.records.map((r) => r.level)).toEqual([30]);
    const again = await run(k);
    expect(again.result.proceed).toBe(true);
    expect(again.records).toEqual([]);
  });

  it('warns and continues while only the old key opens the check', async () => {
    const previous = key();
    await run(previous);
    const { result, records } = await run(key(), previous);
    expect(result.proceed).toBe(true);
    expect(records).toHaveLength(1);
    expect(records[0]?.level).toBe(40);
    expect(records[0]?.msg).toBe(
      'the secrets key check still opens with SECRETS_KEY_OLD only: run rotate-secrets-key',
    );
  });
});
