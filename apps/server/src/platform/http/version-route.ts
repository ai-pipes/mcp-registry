import { VersionResponse } from '@mcp/contracts';
import { Hono } from 'hono';
import {
  type IdentityDeps,
  type IdentityEnv,
  requireSession,
} from '../../modules/identity/index.ts';
import { VERSION } from '../version.ts';

/** `GET /api/version`: the gateway version, for any signed-in person. */
export function versionRoutes(identity: IdentityDeps) {
  return new Hono<IdentityEnv>()
    .use(requireSession(identity))
    .get('/', (c) => c.json(VersionResponse.parse({ version: VERSION })));
}
