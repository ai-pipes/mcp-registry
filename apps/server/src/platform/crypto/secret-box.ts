import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto';
import type { SecretBox } from '../../shared-kernel/secret-box.ts';

const KEY_BYTES = 32;
const IV_BYTES = 12;
const TAG_BYTES = 16;

const checkedKey = (key: Uint8Array): Buffer => {
  if (key.length !== KEY_BYTES) throw new RangeError(`secrets key must be ${KEY_BYTES} bytes`);
  return Buffer.from(key);
};

/**
 * AES-256-GCM. Sealed layout: iv (12) | auth tag (16) | ciphertext. While SECRETS_KEY rotates,
 * `previous` (SECRETS_KEY_OLD) still opens what is not re-sealed yet; sealing always uses `key`.
 */
export class AesGcmSecretBox implements SecretBox {
  private readonly key: Buffer;
  private readonly previous: Buffer | null;

  constructor(key: Uint8Array, opts: { previous?: Uint8Array } = {}) {
    this.key = checkedKey(key);
    this.previous = opts.previous ? checkedKey(opts.previous) : null;
  }

  seal(plain: string): Uint8Array {
    const iv = randomBytes(IV_BYTES);
    const cipher = createCipheriv('aes-256-gcm', this.key, iv);
    const body = Buffer.concat([cipher.update(plain, 'utf8'), cipher.final()]);
    return new Uint8Array(Buffer.concat([iv, cipher.getAuthTag(), body]));
  }

  open(sealed: Uint8Array): string {
    if (sealed.length < IV_BYTES + TAG_BYTES) throw new RangeError('sealed secret is truncated');
    if (this.previous === null) return openWith(this.key, sealed);
    try {
      return openWith(this.key, sealed);
    } catch {
      return openWith(this.previous, sealed);
    }
  }
}

function openWith(key: Buffer, sealed: Uint8Array): string {
  const buf = Buffer.from(sealed);
  const decipher = createDecipheriv('aes-256-gcm', key, buf.subarray(0, IV_BYTES));
  decipher.setAuthTag(buf.subarray(IV_BYTES, IV_BYTES + TAG_BYTES));
  const plain = Buffer.concat([
    decipher.update(buf.subarray(IV_BYTES + TAG_BYTES)),
    decipher.final(),
  ]);
  return plain.toString('utf8');
}
