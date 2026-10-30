import { type ConnectionsDeps, markExpired, resolveCredential } from '../../connections/index.ts';
import type { PersonalCredentials } from '../application/ports.ts';

/** PersonalCredentials over the connections module's public API. */
export function createConnectionCredentials(connections: ConnectionsDeps): PersonalCredentials {
  return {
    resolve: (q) => resolveCredential(connections, q),
    markExpired: (q) => markExpired(connections, q),
  };
}
