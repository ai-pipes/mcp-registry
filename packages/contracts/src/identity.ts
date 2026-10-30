import { z } from 'zod';

export const Role = z.enum(['admin', 'member']);

/** `GET /api/auth/me` */
export const MeResponse = z.object({
  userId: z.string(),
  name: z.string(),
  role: Role,
  groups: z.array(z.string()),
});
export type MeResponse = z.infer<typeof MeResponse>;

/** `GET /api/auth/login?returnTo=` */
export const LoginQuery = z.object({ returnTo: z.string().max(2048).default('/') });

/** `GET /api/auth/callback` — either a code or an error from the provider. */
export const CallbackQuery = z.object({
  state: z.string().min(1).max(256),
  code: z.string().min(1).max(4096),
  /** RFC 9207 issuer identification, sent by providers that advertise support for it. */
  iss: z.string().max(2048).optional(),
});

/** `POST /api/tokens` */
export const CreateTokenRequest = z.object({ name: z.string().trim().min(1).max(100) });
export type CreateTokenRequest = z.infer<typeof CreateTokenRequest>;

export const CreateTokenResponse = z.object({
  id: z.string(),
  prefix: z.string(),
  /** Shown once; only its SHA-256 is stored. */
  token: z.string(),
});
export type CreateTokenResponse = z.infer<typeof CreateTokenResponse>;

export const TokenSummary = z.object({
  id: z.string(),
  name: z.string(),
  prefix: z.string(),
  createdAt: z.string(),
  lastUsedAt: z.string().nullable(),
  revokedAt: z.string().nullable(),
});
export type TokenSummary = z.infer<typeof TokenSummary>;

export const UserSummary = z.object({
  id: z.string(),
  name: z.string(),
  email: z.string().nullable(),
  role: Role,
  groups: z.array(z.string()),
  disabled: z.boolean(),
  createdAt: z.string(),
  lastLoginAt: z.string().nullable(),
});
export type UserSummary = z.infer<typeof UserSummary>;

/** `POST /api/users/:id/disabled` */
export const SetDisabledRequest = z.object({ disabled: z.boolean() });
export type SetDisabledRequest = z.infer<typeof SetDisabledRequest>;
