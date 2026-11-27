import { z } from 'zod';

/** `GET /api/version`: the gateway's own version (the root VERSION file). */
export const VersionResponse = z.object({ version: z.string() });
export type VersionResponse = z.infer<typeof VersionResponse>;
