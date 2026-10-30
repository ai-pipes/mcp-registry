import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vite-plus/test';
import { McpUpstreamClient } from '../../src/platform/upstream/mcp-upstream-client.ts';
import { UpstreamError, type UpstreamTarget } from '../../src/shared-kernel/index.ts';
import { type FakeUpstream, PNG_BASE64, startFakeUpstream } from '../fake-upstream.ts';

const MiB = 1024 * 1024;
let up: FakeUpstream;
const client = new McpUpstreamClient();

const target = (over: Partial<UpstreamTarget> = {}): UpstreamTarget => ({
  url: up.url,
  headers: { Authorization: 'Bearer tok-1', 'X-Extra': 'yes' },
  timeoutMs: 5000,
  maxResponseBytes: MiB,
  ...over,
});

const tools = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    name: `tool_${i}`,
    title: `Tool ${i}`,
    description: `does ${i}`,
    inputSchema: { type: 'object', properties: { a: { type: 'string' } } },
    annotations: { readOnlyHint: true },
  }));

async function failure(p: Promise<unknown>): Promise<UpstreamError> {
  try {
    await p;
  } catch (error) {
    if (error instanceof UpstreamError) return error;
    throw error;
  }
  throw new Error('expected an UpstreamError');
}

beforeAll(async () => {
  up = await startFakeUpstream();
});
afterAll(async () => {
  await up.close();
});
beforeEach(() => {
  up.reset();
  up.setTools(tools(3));
  up.requests.length = 0;
});

describe('McpUpstreamClient', () => {
  it('lists every page of tools/list (nextCursor, 3 pages)', async () => {
    up.setTools(tools(7), 3);
    const listed = await client.listTools(target());
    expect(listed.map((t) => t.name)).toEqual(tools(7).map((t) => t.name));
    expect(listed[0]).toEqual({
      name: 'tool_0',
      title: 'Tool 0',
      description: 'does 0',
      inputSchema: { type: 'object', properties: { a: { type: 'string' } } },
      annotations: { readOnlyHint: true },
    });
    expect(up.requests.filter((r) => r.method === 'tools/list')).toHaveLength(3);
  });

  it('sends target.headers on every request, session set-up and call alike', async () => {
    const r = await client.callTool(target(), 'tool_1', { a: 'b' });
    expect(r.content).toEqual([{ type: 'text', text: '{"a":"b"}' }]);
    expect(r.structuredContent).toEqual({ echo: { a: 'b' } });
    const methods = up.requests.map((x) => x.method);
    expect(methods).toContain('tools/call');
    expect(methods.some((m) => m === 'initialize' || m === 'server/discover')).toBe(true);
    for (const req of up.requests) {
      expect(req.headers.authorization).toBe('Bearer tok-1');
      expect(req.headers['x-extra']).toBe('yes');
    }
  });

  it('does not carry one target’s headers into the next operation', async () => {
    await client.listTools(target());
    up.requests.length = 0;
    await client.listTools(target({ headers: { Authorization: 'Bearer tok-2' } }));
    expect(up.requests.length).toBeGreaterThan(0);
    for (const req of up.requests) {
      expect(req.headers.authorization).toBe('Bearer tok-2');
      expect(req.headers['x-extra']).toBeUndefined();
    }
  });

  it('times out a slow upstream: UpstreamError timeout', async () => {
    up.slow(500);
    const started = Date.now();
    const e = await failure(client.callTool(target({ timeoutMs: 100 }), 'tool_1', {}));
    expect(e.kind).toBe('timeout');
    expect(Date.now() - started).toBeLessThan(450);
    const e2 = await failure(client.listTools(target({ timeoutMs: 100 })));
    expect(e2.kind).toBe('timeout');
  });

  it('refuses a streamed answer above maxResponseBytes promptly: too_large', async () => {
    up.huge(2 * MiB);
    const started = Date.now();
    const e = await failure(
      client.callTool(target({ maxResponseBytes: MiB, timeoutMs: 30_000 }), 'tool_1', {}),
    );
    expect(e.kind).toBe('too_large');
    // Fails as soon as the limit is passed, not when the operation times out.
    expect(Date.now() - started).toBeLessThan(3000);
  });

  it('refuses a JSON answer whose Content-Length is above the limit, before reading it', async () => {
    up.hugeJson(8 * MiB);
    const started = Date.now();
    const e = await failure(
      client.callTool(target({ maxResponseBytes: MiB, timeoutMs: 30_000 }), 'tool_1', {}),
    );
    expect(e.kind).toBe('too_large');
    expect(Date.now() - started).toBeLessThan(3000);
  });

  it('passes a JSON answer with Content-Length within the limit', async () => {
    up.hugeJson(1000);
    const r = await client.callTool(target(), 'tool_1', {});
    expect(r.content).toEqual([{ type: 'text', text: 'x'.repeat(1000) }]);
  });

  it('passes an answer within the limit unchanged', async () => {
    up.huge(MiB / 2);
    const r = await client.callTool(target({ maxResponseBytes: MiB }), 'tool_1', {});
    expect(r.content).toEqual([{ type: 'text', text: 'x'.repeat(MiB / 2) }]);
  });

  it('maps HTTP 401 to unauthorized and 500 to transport', async () => {
    up.status(401);
    expect((await failure(client.listTools(target()))).kind).toBe('unauthorized');
    expect((await failure(client.callTool(target(), 'tool_1', {}))).kind).toBe('unauthorized');
    up.status(500);
    expect((await failure(client.listTools(target()))).kind).toBe('transport');
  });

  it('counts bytes over the whole operation, not per response: many pages add up', async () => {
    const big = { type: 'object', description: 'y'.repeat(100_000) };
    up.setTools(
      Array.from({ length: 30 }, (_, i) => ({ name: `t${i}`, inputSchema: big })),
      1,
    );
    // Each page is ~100 KB, under the 1 MiB limit; the 30 pages together are ~3 MB.
    const e = await failure(client.listTools(target({ maxResponseBytes: MiB })));
    expect(e.kind).toBe('too_large');
  });

  it('does not follow redirects: no credentials reach another host', async () => {
    const other = await startFakeUpstream([{ name: 'a' }]);
    try {
      up.redirect(other.url);
      const e = await failure(client.listTools(target()));
      expect(e.kind).toBe('transport');
      expect(other.requests).toEqual([]);
    } finally {
      await other.close();
    }
  });

  it('maps an unreachable upstream to transport', async () => {
    const e = await failure(client.listTools(target({ url: 'http://127.0.0.1:1/mcp' })));
    expect(e.kind).toBe('transport');
    expect((await failure(client.listTools(target({ url: 'not a url' })))).kind).toBe('transport');
  });

  it('returns an image block byte for byte', async () => {
    up.image();
    const r = await client.callTool(target(), 'tool_1', {});
    expect(r.content[0]).toEqual({ type: 'image', data: PNG_BASE64, mimeType: 'image/png' });
    expect(r.isError).toBeFalsy();
  });

  it('a JSON-RPC error answer (invalid params) is rejected, with its code and message', async () => {
    up.rpcError(-32602, 'Invalid params: owner is required');
    const e = await failure(client.callTool(target(), 'tool_1', {}));
    expect(e.kind).toBe('rejected');
    expect(e.rejection).toEqual({ code: -32602, message: 'Invalid params: owner is required' });
    expect(e.message).not.toContain('owner');
  });

  it('a JSON-RPC internal error (a handler returning err, as mcp-go does) is rejected too', async () => {
    up.rpcError(-32603, 'repository not found');
    const e = await failure(client.callTool(target(), 'tool_1', {}));
    expect(e.kind).toBe('rejected');
    expect(e.rejection?.code).toBe(-32603);
    expect(e.rejection?.message).toContain('repository not found');
    expect((await failure(client.listTools(target()))).kind).toBe('rejected');
  });

  it('error messages carry no credentials', async () => {
    up.status(401);
    const e = await failure(client.listTools(target()));
    expect(e.message).not.toContain('tok-1');
  });
});
