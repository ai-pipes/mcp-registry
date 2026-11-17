import { resealSecret } from '../../shared-kernel/index.ts';
import type { Sql } from '../db.ts';
import { AesGcmSecretBox } from './secret-box.ts';

/** `created`: first start, value stored now. */
export type KeyCheck = 'created' | 'ok' | 'needs_rotation' | 'mismatch';

type Keys = { current: Uint8Array; previous?: Uint8Array };

const PLAIN = 'mcp-registry key check v1';

const opensToPlain = (key: Uint8Array, sealed: Uint8Array): boolean => {
  try {
    return new AesGcmSecretBox(key).open(sealed) === PLAIN;
  } catch {
    return false;
  }
};

/** Pure: what a stored check value says about the configured keys. */
export function judgeKeyCheck(
  sealed: Uint8Array | null,
  keys: Keys,
): Exclude<KeyCheck, 'created'> | 'absent' {
  if (sealed === null) return 'absent';
  if (opensToPlain(keys.current, sealed)) return 'ok';
  if (keys.previous && opensToPlain(keys.previous, sealed)) return 'needs_rotation';
  return 'mismatch';
}

function settled(sealed: Uint8Array, keys: Keys): Exclude<KeyCheck, 'created'> {
  const verdict = judgeKeyCheck(sealed, keys);
  return verdict === 'absent' ? 'mismatch' : verdict;
}

async function readSealed(sql: Sql): Promise<Uint8Array | null> {
  const [row] = await sql<{ sealed: Uint8Array }[]>`select sealed from platform.key_check`;
  return row ? new Uint8Array(row.sealed) : null;
}

/** Reads the check value, creating it when absent; never overwrites an existing one. */
export async function verifySecretsKey(sql: Sql, keys: Keys): Promise<KeyCheck> {
  const existing = await readSealed(sql);
  if (existing !== null) return settled(existing, keys);
  const sealed = new AesGcmSecretBox(keys.current).seal(PLAIN);
  const inserted = await sql`
    insert into platform.key_check (sealed) values (${sealed})
    on conflict (id) do nothing returning id`;
  if (inserted.length > 0) return 'created';
  // A concurrent first start won: judge what it stored.
  const stored = await readSealed(sql);
  return stored === null ? 'mismatch' : settled(stored, keys);
}

/** Re-seals the check value with the current key (used by rotate-secrets-key). */
export async function resealKeyCheck(
  sql: Sql,
  keys: Keys,
): Promise<'current' | 'resealed' | 'unreadable' | 'absent'> {
  const existing = await readSealed(sql);
  if (existing === null) return 'absent';
  if (!keys.previous) return opensToPlain(keys.current, existing) ? 'current' : 'unreadable';
  const r = resealSecret(existing, {
    from: new AesGcmSecretBox(keys.previous),
    to: new AesGcmSecretBox(keys.current),
  });
  if (r.kind !== 'resealed') return r.kind;
  // Compare-and-set: a concurrent run or a start must not be overwritten with stale bytes.
  const updated = await sql`
    update platform.key_check set sealed = ${r.sealed}
    where sealed = ${existing} returning id`;
  return updated.length > 0 ? 'resealed' : 'current';
}
