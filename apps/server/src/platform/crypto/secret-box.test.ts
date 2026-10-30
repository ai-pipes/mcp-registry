import { describe, expect, it } from 'vite-plus/test';
import { AesGcmSecretBox } from './secret-box.ts';

const key = (fill: number) => new Uint8Array(32).fill(fill);

describe('AesGcmSecretBox', () => {
  it('round-trips, including empty and non-ASCII text', () => {
    const box = new AesGcmSecretBox(key(1));
    // The Cyrillic word is written as Unicode escapes on purpose: it covers non-ASCII UTF-8 round-trips.
    for (const text of ['x', '', '\u043f\u0430\u0440\u043e\u043b\u044c-🔑', 'a'.repeat(10_000)]) {
      expect(box.open(box.seal(text))).toBe(text);
    }
  });

  it('uses a random IV: two seals of the same text differ', () => {
    const box = new AesGcmSecretBox(key(1));
    expect(box.seal('x')).not.toEqual(box.seal('x'));
  });

  it('does not contain the plaintext', () => {
    const sealed = new AesGcmSecretBox(key(1)).seal('super-secret-value');
    expect(Buffer.from(sealed).includes('super-secret-value')).toBe(false);
  });

  it('throws when any byte is changed', () => {
    const box = new AesGcmSecretBox(key(1));
    const sealed = box.seal('hello');
    for (const i of [0, 12, 20, sealed.length - 1]) {
      const copy = sealed.slice();
      copy[i] = (copy[i] ?? 0) ^ 1;
      expect(() => box.open(copy), `byte ${i}`).toThrow();
    }
  });

  it('throws on truncated input', () => {
    const box = new AesGcmSecretBox(key(1));
    expect(() => box.open(new Uint8Array(5))).toThrow();
  });

  it('throws under another key', () => {
    const sealed = new AesGcmSecretBox(key(1)).seal('hello');
    expect(() => new AesGcmSecretBox(key(2)).open(sealed)).toThrow();
  });

  it('rejects a key that is not 32 bytes', () => {
    expect(() => new AesGcmSecretBox(new Uint8Array(16))).toThrow(/32 bytes/);
  });

  it('opens with a previous key while SECRETS_KEY rotates, but always seals with the current one', () => {
    const before = new AesGcmSecretBox(key(1)).seal('hello');
    const rotating = new AesGcmSecretBox(key(2), { previous: key(1) });
    expect(rotating.open(before)).toBe('hello');
    const sealed = rotating.seal('hello');
    expect(new AesGcmSecretBox(key(2)).open(sealed)).toBe('hello');
    expect(() => new AesGcmSecretBox(key(1)).open(sealed)).toThrow();
    expect(() => rotating.open(new AesGcmSecretBox(key(3)).seal('x'))).toThrow();
  });
});
