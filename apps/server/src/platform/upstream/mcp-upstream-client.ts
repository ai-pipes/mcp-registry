import {
  Client,
  type FetchLike,
  ProtocolError,
  SdkError,
  SdkErrorCode,
  StreamableHTTPClientTransport,
} from '@modelcontextprotocol/client';
import {
  type ToolCallResult,
  type UpstreamClient,
  UpstreamError,
  type UpstreamTarget,
  type UpstreamTool,
} from '../../shared-kernel/index.ts';

const CLIENT_INFO = { name: 'mcp-registry', version: '1.0.0' };
/** tools/list pages followed at most; a server whose cursor never ends is a transport failure. */
const MAX_PAGES = 100;

/** What the fetch wrapper saw during one operation, to tell failures apart afterwards. */
type Observed = {
  unauthorized: boolean;
  tooLarge: boolean;
  bytes: number;
  /** Aborts the whole operation; fired when the size limit is passed. */
  abort: AbortController;
};

function tooLarge(seen: Observed): UpstreamError {
  seen.tooLarge = true;
  // An errored SSE body alone leaves the pending request waiting for its timeout.
  seen.abort.abort();
  return new UpstreamError('too_large');
}

/**
 * Counts response bytes of the whole operation (every page, every response) as they stream and
 * errors the body once the total passes `limit`, so memory stays bounded whatever upstream sends.
 * A declared Content-Length that would pass it fails early. Passing the limit aborts the whole
 * operation at once. Redirects are refused.
 */
function limitedFetch(limit: number, stop: AbortSignal, seen: Observed): FetchLike {
  return async (input, init) => {
    const signal = init?.signal ? AbortSignal.any([init.signal, stop]) : stop;
    // No redirects: credentials must not follow one to another host.
    const res = await fetch(input, { ...init, signal, redirect: 'error' });
    if (res.status === 401) seen.unauthorized = true;
    const declared = Number(res.headers.get('content-length') ?? Number.NaN);
    if (seen.bytes + declared > limit) {
      const error = tooLarge(seen);
      await res.body?.cancel().catch(() => undefined);
      throw error;
    }
    if (!res.body) return res;
    const counted = res.body.pipeThrough(
      new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          seen.bytes += chunk.byteLength;
          if (seen.bytes > limit) {
            controller.error(tooLarge(seen));
            return;
          }
          controller.enqueue(chunk);
        },
      }),
    );
    return new Response(counted, {
      status: res.status,
      statusText: res.statusText,
      headers: res.headers,
    });
  };
}

function classify(error: unknown, seen: Observed, deadline: AbortSignal): UpstreamError {
  if (seen.tooLarge)
    return new UpstreamError('too_large', 'upstream answer exceeds the size limit');
  if (seen.unauthorized) return new UpstreamError('unauthorized', 'upstream answered 401');
  const timedOut =
    deadline.aborted || (error instanceof SdkError && error.code === SdkErrorCode.RequestTimeout);
  if (timedOut) return new UpstreamError('timeout', 'upstream did not answer in time');
  if (error instanceof UpstreamError) return error;
  // A JSON-RPC error answer: upstream did answer. Its message goes to the agent only.
  if (error instanceof ProtocolError) return UpstreamError.rejected(error.code, error.message);
  // The SDK message may quote the upstream body; only its code is kept.
  const code = error instanceof SdkError ? error.code : 'fetch_failed';
  return new UpstreamError('transport', `upstream transport error (${code})`);
}

/**
 * UpstreamClient on the official MCP SDK (Streamable HTTP). One MCP session per operation:
 * connect → request(s) → close, so no session or credential outlives the operation.
 * `timeoutMs` bounds the whole operation, set-up included.
 */
export class McpUpstreamClient implements UpstreamClient {
  listTools(t: UpstreamTarget): Promise<UpstreamTool[]> {
    return this.session(t, async (client, opts) => {
      // An explicit cursor (even `{}` for the first page) makes the SDK return one raw page.
      const pages = async (cursor: string | undefined, n: number): Promise<UpstreamTool[]> => {
        if (n >= MAX_PAGES)
          throw new UpstreamError('transport', 'tools/list pagination does not end');
        const res = await client.listTools(cursor === undefined ? {} : { cursor }, opts);
        const tools = res.tools.map(toUpstreamTool);
        return res.nextCursor ? [...tools, ...(await pages(res.nextCursor, n + 1))] : tools;
      };
      return pages(undefined, 0);
    });
  }

  callTool(
    t: UpstreamTarget,
    name: string,
    args: Record<string, unknown>,
  ): Promise<ToolCallResult> {
    return this.session(t, async (client, opts) => {
      const res = await client.callTool({ name, arguments: args }, opts);
      const out: ToolCallResult = { content: res.content };
      const structured = res.structuredContent;
      if (isRecord(structured)) out.structuredContent = structured;
      if (res.isError !== undefined) out.isError = res.isError;
      return out;
    });
  }

  private async session<T>(
    t: UpstreamTarget,
    run: (client: Client, opts: { signal: AbortSignal; timeout: number }) => Promise<T>,
  ): Promise<T> {
    const deadline = AbortSignal.timeout(t.timeoutMs);
    const seen: Observed = {
      unauthorized: false,
      tooLarge: false,
      bytes: 0,
      abort: new AbortController(),
    };
    const stop = AbortSignal.any([deadline, seen.abort.signal]);
    const client = new Client(CLIENT_INFO, { capabilities: {} });
    const opts = { signal: stop, timeout: t.timeoutMs, maxTotalTimeout: t.timeoutMs };
    try {
      const transport = new StreamableHTTPClientTransport(new URL(t.url), {
        requestInit: { headers: { ...t.headers } },
        fetch: limitedFetch(t.maxResponseBytes, stop, seen),
      });
      await client.connect(transport, opts);
      return await run(client, opts);
    } catch (error) {
      throw classify(error, seen, deadline);
    } finally {
      await client.close().catch(() => undefined);
    }
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function toUpstreamTool(tool: {
  name: string;
  title?: string | undefined;
  description?: string | undefined;
  inputSchema: Record<string, unknown>;
  outputSchema?: Record<string, unknown> | undefined;
  annotations?: Record<string, unknown> | undefined;
}): UpstreamTool {
  const out: UpstreamTool = { name: tool.name, inputSchema: tool.inputSchema };
  if (tool.title !== undefined) out.title = tool.title;
  if (tool.description !== undefined) out.description = tool.description;
  if (tool.outputSchema !== undefined) out.outputSchema = tool.outputSchema;
  if (tool.annotations !== undefined) out.annotations = tool.annotations;
  return out;
}
