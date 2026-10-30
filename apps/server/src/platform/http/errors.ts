import type { Context } from 'hono';
import type { ContentfulStatusCode } from 'hono/utils/http-status';
import { DomainError } from '../../shared-kernel/index.ts';

/** HTTP status per domain error code; modules add their codes here. */
const STATUS: Record<string, ContentfulStatusCode> = {
  not_found: 404,
  invalid_token: 401,
  unauthenticated: 401,
  forbidden: 403,
  csrf: 403,
  limit_exceeded: 413,
  rate_limited: 429,
  server_exists: 409,
  server_conflict: 409,
  sync_failed: 502,
};

function statusFor(code: string): ContentfulStatusCode {
  return STATUS[code] ?? (code.startsWith('invalid_') ? 422 : 400);
}

export function errorResponse(c: Context, code: string, message: string): Response {
  return c.json({ error: { code, message } }, statusFor(code));
}

/** JSON error response for a domain error. */
export function domainErrorResponse(c: Context, e: DomainError): Response {
  return errorResponse(c, e.code, e.message);
}

export const invalidRequest = (message: string) => new DomainError('invalid_request', message);
