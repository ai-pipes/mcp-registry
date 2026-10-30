import { ErrorBody } from '@mcp/contracts';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vite-plus/test';
import { z } from 'zod';
import { errorResponse, server } from '../../test/msw/handlers.ts';
import { api, onUnauthorized } from './client.ts';
import { ApiError, isApiError } from './errors.ts';

const Item = z.object({ id: z.string() });

async function caught(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise;
  } catch (e) {
    if (isApiError(e)) return e;
    throw e;
  }
  throw new Error('expected the call to fail');
}

describe('api client', () => {
  const unsubscribe: Array<() => void> = [];
  afterEach(() => {
    for (const off of unsubscribe.splice(0)) off();
  });

  it('get validates the response with the schema and returns the data', async () => {
    server.use(http.get('/api/items', () => HttpResponse.json({ id: 'a' })));
    await expect(api.get('/api/items', Item)).resolves.toEqual({ id: 'a' });
  });

  it('a response that fails the schema is a bad_response error', async () => {
    server.use(http.get('/api/items', () => HttpResponse.json({ id: 1 })));
    const error = await caught(api.get('/api/items', Item));
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 200, code: 'bad_response' });
  });

  it('a non-2xx ErrorBody becomes ApiError with its status and code', async () => {
    server.use(http.put('/api/items/a', () => errorResponse(409, 'conflict', 'stale')));
    const error = await caught(api.put('/api/items/a', { v: 1 }));
    expect(error).toMatchObject({ status: 409, code: 'conflict', message: 'stale' });
    expect(
      ErrorBody.safeParse({ error: { code: error.code, message: error.message } }).success,
    ).toBe(true);
  });

  it('a 429 carries Retry-After as retryAfterSec', async () => {
    server.use(
      http.post('/api/items', () => {
        const res = errorResponse(429, 'rate_limited');
        res.headers.set('Retry-After', '300');
        return res;
      }),
    );
    const error = await caught(api.post('/api/items', { a: 1 }));
    expect(error).toMatchObject({ status: 429, code: 'rate_limited', retryAfterSec: 300 });
  });

  it('an error without Retry-After has no retryAfterSec', async () => {
    server.use(http.put('/api/items/a', () => errorResponse(409, 'conflict')));
    const error = await caught(api.put('/api/items/a', { v: 1 }));
    expect(error.retryAfterSec).toBeUndefined();
  });

  it('a non-JSON error body becomes http_<status>', async () => {
    server.use(http.get('/api/items', () => new HttpResponse('gateway down', { status: 502 })));
    const error = await caught(api.get('/api/items', Item));
    expect(error).toMatchObject({ status: 502, code: 'http_502' });
  });

  it('401 notifies the onUnauthorized subscribers', async () => {
    const listener = vi.fn();
    unsubscribe.push(onUnauthorized(listener));
    server.use(http.get('/api/items', () => errorResponse(401, 'unauthenticated')));
    const error = await caught(api.get('/api/items', Item));
    expect(error).toMatchObject({ status: 401, code: 'unauthenticated' });
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it('sends JSON bodies with same-origin credentials', async () => {
    let seen: { body: unknown; type: string | null; credentials: string } | undefined;
    server.use(
      http.post('/api/items', async ({ request }) => {
        seen = {
          body: await request.json(),
          type: request.headers.get('content-type'),
          credentials: request.credentials,
        };
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await expect(api.post('/api/items', { name: 'ci' })).resolves.toBeUndefined();
    expect(seen).toEqual({
      body: { name: 'ci' },
      type: 'application/json',
      credentials: 'same-origin',
    });
  });

  it('delete resolves on 204', async () => {
    server.use(http.delete('/api/items/t1', () => new HttpResponse(null, { status: 204 })));
    await expect(api.delete('/api/items/t1')).resolves.toBeUndefined();
  });

  it('a request that does not reach the server is a network_error', async () => {
    server.use(http.get('/api/items', () => HttpResponse.error()));
    const error = await caught(api.get('/api/items', Item));
    expect(error).toMatchObject({ status: 0, code: 'network_error' });
  });
});
