import { createHash, randomBytes } from 'node:crypto';
import type { Pkce } from '../application/ports.ts';

export const nodePkce: Pkce = {
  // 32 random bytes → 43 base64url characters, the RFC 7636 minimum length for a verifier.
  randomToken: () => randomBytes(32).toString('base64url'),
  challenge: (verifier) => createHash('sha256').update(verifier, 'ascii').digest('base64url'),
};
