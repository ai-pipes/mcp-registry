import { z } from 'zod';

const HttpUrl = z
  .url()
  .max(2048)
  .refine((u) => /^https?:\/\//i.test(u), 'must be an http(s) URL');

const Header = z.object({
  header: z.string().min(1).max(100),
  /** Contains `{secret}`, e.g. `Bearer {secret}`. */
  template: z.string().min(1).max(500),
});

/** OAuth client settings; the client secret is set separately and never read back. */
export const OAuthConfig = z.object({
  authorizeUrl: HttpUrl,
  tokenUrl: HttpUrl,
  revokeUrl: HttpUrl.optional(),
  clientId: z.string().min(1).max(500),
  scopes: z.array(z.string().min(1).max(200)).max(50),
  extraAuthorizeParams: z.record(z.string().max(100), z.string().max(1000)).optional(),
});
export type OAuthConfig = z.infer<typeof OAuthConfig>;

export const AuthInput = z.discriminatedUnion('kind', [
  z.object({ kind: z.literal('none') }),
  z.object({ kind: z.literal('shared_secret'), ...Header.shape }),
  z.object({ kind: z.literal('pat_user'), ...Header.shape }),
  z.object({ kind: z.literal('oauth_user'), oauth: OAuthConfig, ...Header.shape }),
]);
export type AuthInput = z.infer<typeof AuthInput>;

export const AccessInput = z.discriminatedUnion('kind', [
  z.object({ kind: z.literal('all') }),
  z.object({
    kind: z.literal('groups'),
    groups: z.array(z.string().min(1).max(200)).min(1).max(100),
  }),
]);
export type AccessInput = z.infer<typeof AccessInput>;

export const ServerStatus = z.enum(['active', 'disabled']);

/** The editable part of a server. */
const ServerFields = z.object({
  name: z.string().min(1).max(100),
  description: z.string().max(500),
  url: HttpUrl,
  auth: AuthInput,
  access: AccessInput,
  timeoutMs: z.number().int().min(1000).max(600_000).default(120_000),
  dailyLimit: z.number().int().min(1).max(100_000).nullable().default(null),
});

/** `POST /api/servers`; `slug` is the tool-name prefix and cannot change later. */
export const ServerInput = ServerFields.extend({ slug: z.string().min(1).max(64) });
export type ServerInput = z.infer<typeof ServerInput>;

/**
 * `POST /api/servers` body: the input plus the secret for a mode that holds one (shared key or
 * OAuth client secret). The secret is write-only.
 */
export const CreateServerRequest = ServerInput.extend({
  secret: z.string().min(1).max(4096).nullish(),
});
export type CreateServerRequest = z.infer<typeof CreateServerRequest>;

/** `PUT /api/servers/:slug`: everything but the slug. */
export const UpdateServerRequest = ServerFields;
export type UpdateServerRequest = z.infer<typeof UpdateServerRequest>;

/** `PUT /api/servers/:slug/secret`; `null` clears. */
export const SetSecretRequest = z.object({ secret: z.string().min(1).max(4096).nullable() });
export type SetSecretRequest = z.infer<typeof SetSecretRequest>;

/** `POST /api/servers/:slug/status` */
export const SetStatusRequest = z.object({ status: ServerStatus });
export type SetStatusRequest = z.infer<typeof SetStatusRequest>;

/** Admin view of a server. Holds `secretSet` only: no secret and no OAuth client secret. */
export const ServerView = z.object({
  slug: z.string(),
  name: z.string(),
  description: z.string(),
  url: z.string(),
  auth: AuthInput,
  secretSet: z.boolean(),
  access: AccessInput,
  timeoutMs: z.number().int(),
  dailyLimit: z.number().int().nullable(),
  status: ServerStatus,
  lastSyncAt: z.string().nullable(),
  lastSyncError: z.string().nullable(),
  toolCount: z.number().int(),
});
export type ServerView = z.infer<typeof ServerView>;

export const ServerList = z.array(ServerView);

/** `POST /api/servers/:slug/sync` */
export const SyncResult = z.object({
  added: z.number().int(),
  changed: z.number().int(),
  removed: z.number().int(),
});
export type SyncResult = z.infer<typeof SyncResult>;
