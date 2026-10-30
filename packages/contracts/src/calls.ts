import { z } from 'zod';

export const CallOutcome = z.enum([
  'ok',
  'upstream_error',
  'timeout',
  'transport_error',
  'denied:not_found',
  'denied:invalid_arguments',
  'denied:quota',
  'denied:needs_connection',
  'denied:expired',
]);
export type CallOutcome = z.infer<typeof CallOutcome>;

export const DEFAULT_CALLS_LIMIT = 100;
export const MAX_CALLS_LIMIT = 500;

/** `GET /api/calls` query; newest first, `before` pages back in time. */
export const CallsQuery = z.object({
  user: z.uuid().optional(),
  server: z.string().min(1).max(200).optional(),
  outcome: CallOutcome.optional(),
  before: z.iso.datetime({ offset: true }).optional(),
  limit: z.coerce.number().int().min(1).max(MAX_CALLS_LIMIT).default(DEFAULT_CALLS_LIMIT),
});
export type CallsQuery = z.infer<typeof CallsQuery>;

/** One row of the call log: who, what, when, how it ended. Never arguments or results. */
export const CallEntry = z.object({
  id: z.string(),
  userId: z.string(),
  tokenId: z.string().nullable(),
  server: z.string(),
  tool: z.string(),
  startedAt: z.string(),
  durationMs: z.number().int(),
  outcome: CallOutcome,
});
export type CallEntry = z.infer<typeof CallEntry>;

export const CallList = z.array(CallEntry);
