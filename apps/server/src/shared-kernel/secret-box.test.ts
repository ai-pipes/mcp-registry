import { describe, expect, it } from 'vite-plus/test';
import { resealSecret, type SecretBox } from './secret-box.ts';

/** A box per key id: sealed bytes are `<id>:<plain>`; open refuses another id. */
const box = (id: string): SecretBox => ({
  seal: (plain) => new TextEncoder().encode(`${id}:${plain}`),
  open: (sealed) => {
    const text = new TextDecoder().decode(sealed);
    if (!text.startsWith(`${id}:`)) throw new Error('wrong key');
    return text.slice(id.length + 1);
  },
});
const text = (b: Uint8Array) => new TextDecoder().decode(b);

describe('resealSecret', () => {
  const keys = { from: box('old'), to: box('new') };

  it('re-seals a secret sealed with the old key under the new one', () => {
    const r = resealSecret(keys.from.seal('s3cret'), keys);
    expect(r.kind).toBe('resealed');
    if (r.kind === 'resealed') expect(text(r.sealed)).toBe('new:s3cret');
  });

  it('leaves a secret that the new key already opens (a repeated run)', () => {
    expect(resealSecret(keys.to.seal('s3cret'), keys)).toEqual({ kind: 'current' });
  });

  it('reports a secret that neither key opens', () => {
    expect(resealSecret(box('other').seal('s3cret'), keys)).toEqual({ kind: 'unreadable' });
  });
});
