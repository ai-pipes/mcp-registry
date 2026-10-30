import { z } from 'zod';

export const ConnectionStatus = z.enum(['ready', 'expired', 'missing']);

/** One row of `GET /api/connections`: a usable server that needs the person's own credentials. */
export const ConnectionSummary = z.object({
  server: z.string(),
  name: z.string(),
  authKind: z.enum(['oauth_user', 'pat_user']),
  status: ConnectionStatus,
});
export type ConnectionSummary = z.infer<typeof ConnectionSummary>;

/** `POST /api/connections/:server/start` → where to send the browser. */
export const StartConnectResponse = z.object({ url: z.string() });
export type StartConnectResponse = z.infer<typeof StartConnectResponse>;

/** `PUT /api/connections/:server/pat` */
export const SavePatRequest = z.object({ token: z.string().min(1).max(4096) });
export type SavePatRequest = z.infer<typeof SavePatRequest>;

/** `GET /api/connections/callback` from the upstream's OAuth provider. */
export const ConnectCallbackQuery = z.object({
  state: z.string().min(1).max(256),
  code: z.string().min(1).max(4096),
});

/**
 * `?error=` of the redirect to `/connections` after the callback: a fixed set, nothing from the
 * request is reflected.
 */
export const ConnectCallbackError = z.enum([
  'unauthenticated',
  'denied',
  'invalid_state',
  'not_found',
  'invalid_auth_kind',
  'exchange_failed',
  /** An unexpected failure in the gateway (e.g. the database is down); details only in the log. */
  'internal',
]);
export type ConnectCallbackError = z.infer<typeof ConnectCallbackError>;
