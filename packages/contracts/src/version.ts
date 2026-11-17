import { z } from 'zod';

/** `GET /api/version`: the gateway's own version (apps/server/package.json). */
export const VersionResponse = z.object({ version: z.string() });
export type VersionResponse = z.infer<typeof VersionResponse>;
