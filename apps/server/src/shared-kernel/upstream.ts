/**
 * Port to an upstream MCP server (Streamable HTTP). The implementation lives in
 * `platform/upstream`; catalog (synchronisation) and gateway (`call_tool`) use it.
 */

/** Where and how to reach an upstream for one operation. Headers already carry the credentials. */
export interface UpstreamTarget {
  url: string;
  headers: Record<string, string>;
  timeoutMs: number;
  maxResponseBytes: number;
}

/** A tool as upstream `tools/list` describes it. */
export interface UpstreamTool {
  name: string;
  title?: string;
  description?: string;
  inputSchema: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
  annotations?: Record<string, unknown>;
}

/**
 * Upstream `tools/call` result as is; the gateway hands it to the agent unchanged. Known limit of
 * the SDK-based client: a non-object `structuredContent` and the result's `_meta` are dropped
 * (spec-conforming upstreams send `structuredContent` as an object).
 */
export interface ToolCallResult {
  content: unknown[];
  structuredContent?: Record<string, unknown>;
  isError?: boolean;
}

/**
 * `rejected`: upstream answered the request with a JSON-RPC error (unknown tool, invalid params,
 * an internal error of its handler) — an answer, not a transport failure.
 */
export type UpstreamErrorKind = 'unauthorized' | 'timeout' | 'too_large' | 'transport' | 'rejected';

/** The JSON-RPC error upstream answered with. `message` is upstream's text: for the agent only. */
export type UpstreamRejection = { code: number; message: string };

/**
 * Upstream could not be used. The message carries no upstream body, headers or credentials; a
 * JSON-RPC error's own text is kept apart in `rejection`, never in the message (logs drop
 * messages anyway, but nothing upstream wrote may reach them).
 */
export class UpstreamError extends Error {
  readonly rejection: UpstreamRejection | null;

  constructor(
    readonly kind: UpstreamErrorKind,
    message: string = kind,
    rejection: UpstreamRejection | null = null,
  ) {
    super(message);
    this.name = 'UpstreamError';
    this.rejection = rejection;
  }

  /** Upstream answered with a JSON-RPC error. */
  static rejected(code: number, message: string): UpstreamError {
    return new UpstreamError('rejected', `upstream rejected the request (JSON-RPC ${code})`, {
      code,
      message,
    });
  }
}

/**
 * Each operation is its own MCP session (connect → request → close): nothing, credentials
 * included, is shared between operations.
 */
export interface UpstreamClient {
  /** Every page of `tools/list`. */
  listTools(t: UpstreamTarget): Promise<UpstreamTool[]>;
  callTool(t: UpstreamTarget, name: string, args: Record<string, unknown>): Promise<ToolCallResult>;
}
