/** Port: symmetric authenticated encryption for secrets stored in the database. */
export interface SecretBox {
  seal(plain: string): Uint8Array;
  /** Throws when the bytes were altered or sealed with another key. */
  open(sealed: Uint8Array): string;
}

/** What re-sealing one stored secret under a new key came to. */
export type Resealed =
  | { kind: 'resealed'; sealed: Uint8Array }
  /** Already sealed with the new key, e.g. by a previous, interrupted run: nothing to do. */
  | { kind: 'current' }
  /** Neither key opens it. */
  | { kind: 'unreadable' };

const opens = (box: SecretBox, sealed: Uint8Array): string | null => {
  try {
    return box.open(sealed);
  } catch {
    return null;
  }
};

/**
 * Key rotation of one stored secret: `from` is the old key, `to` the new one. Trying the new key
 * first makes a repeated run harmless. The plain text never leaves this function.
 */
export function resealSecret(
  sealed: Uint8Array,
  keys: { from: SecretBox; to: SecretBox },
): Resealed {
  if (opens(keys.to, sealed) !== null) return { kind: 'current' };
  const plain = opens(keys.from, sealed);
  return plain === null
    ? { kind: 'unreadable' }
    : { kind: 'resealed', sealed: keys.to.seal(plain) };
}
