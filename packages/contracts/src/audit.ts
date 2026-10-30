import { z } from 'zod';

/** `GET /api/audit` query; newest first, `before` is an entry id to page back from. */
export const AuditQuery = z.object({
  before: z
    .string()
    .regex(/^\d{1,18}$/)
    .optional(),
  limit: z.coerce.number().int().min(1).max(500).default(100),
});
export type AuditQuery = z.infer<typeof AuditQuery>;

/** An admin action. `meta` never carries secret values. */
export const AuditEntry = z.object({
  id: z.string(),
  at: z.string(),
  actorId: z.string().nullable(),
  action: z.string(),
  target: z.string().nullable(),
  meta: z.record(z.string(), z.unknown()),
});
export type AuditEntry = z.infer<typeof AuditEntry>;

export const AuditList = z.array(AuditEntry);
