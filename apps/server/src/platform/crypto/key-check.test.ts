import { randomBytes } from 'node:crypto';
import { describe, expect, it } from 'vite-plus/test';
import { judgeKeyCheck } from './key-check.ts';
import { AesGcmSecretBox } from './secret-box.ts';

const key = () => new Uint8Array(randomBytes(32));
const PLAIN = 'mcp-registry key check v1';

describe('judgeKeyCheck', () => {
  it('is absent when nothing is stored', () => {
    expect(judgeKeyCheck(null, { current: key() })).toBe('absent');
  });

  it('is ok when the current key opens it', () => {
    const current = key();
    const sealed = new AesGcmSecretBox(current).seal(PLAIN);
    expect(judgeKeyCheck(sealed, { current })).toBe('ok');
    expect(judgeKeyCheck(sealed, { current, previous: key() })).toBe('ok');
  });

  it('needs rotation when only the previous key opens it', () => {
    const previous = key();
    const sealed = new AesGcmSecretBox(previous).seal(PLAIN);
    expect(judgeKeyCheck(sealed, { current: key(), previous })).toBe('needs_rotation');
  });

  it('is a mismatch when no configured key opens it, also without a previous key', () => {
    const sealed = new AesGcmSecretBox(key()).seal(PLAIN);
    expect(judgeKeyCheck(sealed, { current: key() })).toBe('mismatch');
    expect(judgeKeyCheck(sealed, { current: key(), previous: key() })).toBe('mismatch');
  });

  it('is a mismatch for a truncated value', () => {
    const current = key();
    const sealed = new AesGcmSecretBox(current).seal(PLAIN);
    expect(judgeKeyCheck(sealed.slice(0, 10), { current })).toBe('mismatch');
  });

  it('is a mismatch when it opens but holds another text', () => {
    const current = key();
    const sealed = new AesGcmSecretBox(current).seal('something else');
    expect(judgeKeyCheck(sealed, { current })).toBe('mismatch');
  });
});
