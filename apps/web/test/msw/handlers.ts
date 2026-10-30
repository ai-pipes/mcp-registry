import type { ErrorBody } from '@mcp/contracts';
import { HttpResponse } from 'msw';
import { setupServer } from 'msw/node';

/** `ErrorBody` JSON response, as the server sends for every non-2xx. */
export function errorResponse(status: number, code: string, message = code) {
  const body: ErrorBody = { error: { code, message } };
  return HttpResponse.json(body, { status });
}

/** The msw server shared by all web tests (started in test/setup.ts); tests add `server.use(...)`. */
export const server = setupServer();
