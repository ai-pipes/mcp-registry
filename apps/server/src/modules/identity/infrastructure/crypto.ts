import { createHash, randomBytes, timingSafeEqual } from 'node:crypto';
import type { Sha256 } from '../application/ports.ts';

export const nodeSha256: Sha256 = {
  hex: (input) => createHash('sha256').update(input, 'utf8').digest('hex'),
  base64url: (input) => createHash('sha256').update(input, 'utf8').digest('base64url'),
  timingSafeEqual: (a, b) => {
    const x = createHash('sha256').update(a, 'utf8').digest();
    const y = createHash('sha256').update(b, 'utf8').digest();
    return timingSafeEqual(x, y);
  },
};

export const cryptoRandom = {
  base64url: (bytes: number): string => randomBytes(bytes).toString('base64url'),
};
