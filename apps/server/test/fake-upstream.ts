import { serve } from '@hono/node-server';
import { createMcpHandler, ProtocolError, Server, type Tool } from '@modelcontextprotocol/server';

export type FakeTool = {
  name: string;
  title?: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
  annotations?: Record<string, unknown>;
};

type RecordedRequest = { method: string | null; headers: Record<string, string> };

/** 1×1 transparent PNG. */
export const PNG_BASE64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';

type Mode =
  | { kind: 'normal' }
  | { kind: 'slow'; ms: number }
  | { kind: 'huge'; bytes: number }
  | { kind: 'status'; code: 401 | 500 }
  | { kind: 'image' }
  | { kind: 'redirect'; to: string }
  | { kind: 'hugeJson'; bytes: number }
  | { kind: 'rpcError'; code: number; message: string };

export interface FakeUpstream {
  url: string;
  /** Every HTTP request received, with its JSON-RPC method (null for non-JSON bodies). */
  requests: RecordedRequest[];
  /** Tools served by `tools/list`, `pageSize` per page (cursor = start index). */
  setTools(tools: FakeTool[], pageSize?: number): void;
  /** `tools/list` and `tools/call` answer after `ms`. */
  slow(ms: number): void;
  /** `tools/call` answers with a text block of about `bytes` bytes. */
  huge(bytes: number): void;
  /** Every HTTP request is answered with this status (401 with `WWW-Authenticate`). */
  status(code: 401 | 500): void;
  /** `tools/call` answers `{ type: 'image', data: PNG_BASE64, mimeType: 'image/png' }`. */
  image(): void;
  /**
   * `tools/call` is answered with a plain `application/json` body of about `bytes` bytes and its
   * Content-Length (not SSE), so a client can refuse it from the header alone.
   */
  hugeJson(bytes: number): void;
  /** `tools/list` and `tools/call` answer with this JSON-RPC error (as a handler throwing it). */
  rpcError(code: number, message: string): void;
  /** Every HTTP request is answered `307` to `to`. */
  redirect(to: string): void;
  /** Back to normal answers (`tools/call` echoes its arguments as text). */
  reset(): void;
  close(): Promise<void>;
}

function asTool(t: FakeTool): Tool {
  const tool: Tool = { name: t.name, inputSchema: { type: 'object', ...t.inputSchema } };
  if (t.title !== undefined) tool.title = t.title;
  if (t.description !== undefined) tool.description = t.description;
  if (t.outputSchema !== undefined) tool.outputSchema = { type: 'object', ...t.outputSchema };
  if (t.annotations !== undefined) tool.annotations = t.annotations;
  return tool;
}

const sleep = (ms: number) =>
  new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });

type FakeUpstreamOptions = {
  /** Default 127.0.0.1; the smoke test's container listens on 0.0.0.0. */
  hostname?: string;
  /** Default 0: a random free port. */
  port?: number;
  /** Every request without exactly this header value is answered 401, as a real upstream would. */
  requireHeader?: { name: string; value: string };
};

/** An in-process upstream MCP server (Streamable HTTP), by default on a random localhost port. */
export async function startFakeUpstream(
  initial: FakeTool[] = [],
  opts: FakeUpstreamOptions = {},
): Promise<FakeUpstream> {
  let tools = initial;
  let pageSize = Number.POSITIVE_INFINITY;
  let mode: Mode = { kind: 'normal' };
  const requests: RecordedRequest[] = [];

  const delay = async () => {
    if (mode.kind === 'slow') await sleep(mode.ms);
  };

  const factory = () => {
    const server = new Server(
      { name: 'fake-upstream', version: '1.0.0' },
      { capabilities: { tools: {} } },
    );
    server.setRequestHandler('tools/list', async (request) => {
      await delay();
      if (mode.kind === 'rpcError') throw new ProtocolError(mode.code, mode.message);
      const start = Number(request.params?.cursor ?? 0);
      const end = Math.min(start + pageSize, tools.length);
      const page = tools.slice(start, end).map(asTool);
      return end < tools.length ? { tools: page, nextCursor: String(end) } : { tools: page };
    });
    server.setRequestHandler('tools/call', async (request) => {
      await delay();
      if (mode.kind === 'rpcError') throw new ProtocolError(mode.code, mode.message);
      if (mode.kind === 'huge') {
        return { content: [{ type: 'text', text: 'x'.repeat(mode.bytes) }] };
      }
      if (mode.kind === 'image') {
        return { content: [{ type: 'image', data: PNG_BASE64, mimeType: 'image/png' }] };
      }
      return {
        content: [{ type: 'text', text: JSON.stringify(request.params.arguments ?? {}) }],
        structuredContent: { echo: request.params.arguments ?? {} },
      };
    });
    return server;
  };
  // Large tool arguments are the gateway's to limit, not this fake's (the SDK default is 4 MiB).
  const handler = createMcpHandler(factory, { maxRequestBodySize: 64 * 1024 * 1024 });

  const fetch = async (req: Request): Promise<Response> => {
    let method: string | null = null;
    let id: unknown = null;
    try {
      const body: unknown = await req.clone().json();
      if (body !== null && typeof body === 'object' && 'method' in body) {
        method = typeof body.method === 'string' ? body.method : null;
        id = 'id' in body ? body.id : null;
      }
    } catch {
      // Not JSON (GET, DELETE): recorded with method null.
    }
    requests.push({ method, headers: Object.fromEntries(req.headers) });
    const required = opts.requireHeader;
    if (required && req.headers.get(required.name) !== required.value) {
      return new Response('unauthorized', {
        status: 401,
        headers: { 'WWW-Authenticate': 'Bearer error="invalid_token"' },
      });
    }
    if (mode.kind === 'hugeJson' && method === 'tools/call') {
      const text = 'x'.repeat(mode.bytes);
      const body = JSON.stringify({
        jsonrpc: '2.0',
        id,
        result: { content: [{ type: 'text', text }] },
      });
      return new Response(body, {
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': String(Buffer.byteLength(body)),
        },
      });
    }
    if (mode.kind === 'redirect') {
      return new Response(null, { status: 307, headers: { Location: mode.to } });
    }
    if (mode.kind === 'status') {
      const headers: Record<string, string> =
        mode.code === 401 ? { 'WWW-Authenticate': 'Bearer error="invalid_token"' } : {};
      return new Response('nope', { status: mode.code, headers });
    }
    return handler.fetch(req);
  };

  const hostname = opts.hostname ?? '127.0.0.1';
  const server = serve({ fetch, hostname, port: opts.port ?? 0 });
  await new Promise<void>((resolve) => {
    if (server.listening) resolve();
    else server.once('listening', () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('no TCP address');
  const { port } = address;

  return {
    url: `http://${hostname === '0.0.0.0' ? '127.0.0.1' : hostname}:${port}/mcp`,
    requests,
    setTools(next, size = Number.POSITIVE_INFINITY) {
      tools = next;
      pageSize = size;
    },
    slow(ms) {
      mode = { kind: 'slow', ms };
    },
    huge(bytes) {
      mode = { kind: 'huge', bytes };
    },
    status(code) {
      mode = { kind: 'status', code };
    },
    image() {
      mode = { kind: 'image' };
    },
    hugeJson(bytes) {
      mode = { kind: 'hugeJson', bytes };
    },
    rpcError(code, message) {
      mode = { kind: 'rpcError', code, message };
    },
    redirect(to) {
      mode = { kind: 'redirect', to };
    },
    reset() {
      mode = { kind: 'normal' };
    },
    async close() {
      await handler.close();
      if ('closeAllConnections' in server) server.closeAllConnections();
      await new Promise<void>((resolve) => {
        server.close(() => resolve());
      });
    },
  };
}
