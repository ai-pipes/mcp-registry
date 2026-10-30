import { Hono } from 'hono';
import { describe, expect, it } from 'vite-plus/test';
import { z } from 'zod';
import { DomainError } from '../../src/shared-kernel/index.ts';
import { readJson, readQuery } from '../../src/platform/http/request.ts';
import { domainErrorResponse } from '../../src/platform/http/errors.ts';

const Body = z.object({ name: z.string().min(1) });

const app = new Hono()
  .onError((e, c) => (e instanceof DomainError ? domainErrorResponse(c, e) : c.text('boom', 500)))
  .post('/json', async (c) => c.json(await readJson(c, Body)))
  .get('/query', (c) => c.json(readQuery(c, z.object({ q: z.string() }))));

describe('request parsing', () => {
  it('parses a JSON body against the schema', async () => {
    const res = await app.request('/json', { method: 'POST', body: '{"name":"a"}' });
    expect(await res.json()).toEqual({ name: 'a' });
  });

  it('answers 422 invalid_request with the field path for a bad body', async () => {
    const res = await app.request('/json', { method: 'POST', body: '{"name":""}' });
    expect(res.status).toBe(422);
    expect(await res.json()).toMatchObject({ error: { code: 'invalid_request' } });
  });

  it('rejects a body that is not JSON', async () => {
    const res = await app.request('/json', { method: 'POST', body: 'not json' });
    expect(await res.json()).toEqual({
      error: { code: 'invalid_request', message: 'body is not valid JSON' },
    });
  });

  it('parses the query string', async () => {
    expect(await (await app.request('/query?q=x')).json()).toEqual({ q: 'x' });
    expect((await app.request('/query')).status).toBe(422);
  });
});
