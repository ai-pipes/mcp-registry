import { AuthMode, type ServerCallTarget, ToolRef } from '../../catalog/index.ts';
import {
  err,
  errorFields,
  ok,
  type Result,
  type ToolCallResult,
  UpstreamError,
  type UpstreamErrorKind,
  type UpstreamRejection,
} from '../../../shared-kernel/index.ts';
import { CallOutcome, onUpstreamUnauthorized } from '../domain/call-outcome.ts';
import { QuotaDay } from '../domain/quota-day.ts';
import type { GatewayDeps, Principal } from './ports.ts';

const MIB = 1024 * 1024;
/** Server and tool names come from the agent's ref: the log keeps at most this many characters. */
const MAX_LOGGED_NAME = 200;
const bounded = (name: string) => name.slice(0, MAX_LOGGED_NAME);
const round = (n: number) => Number(n.toFixed(2));
/** Upstream's own JSON-RPC error text is passed to the agent cut to this many characters. */
const MAX_REJECTION_TEXT = 500;
const cut = (text: string) =>
  text.length > MAX_REJECTION_TEXT ? `${text.slice(0, MAX_REJECTION_TEXT - 1)}…` : text;

/** Texts for the agent (`isError: true`). No internal details: no URLs, statuses or upstream bodies. */
export const CALL_TOOL_TEXT = {
  notFound: (ref: string) => `Tool "${ref}" is not available. Use search_tools to find another.`,
  invalidArguments: (ref: string, path: string, message: string) =>
    `Invalid arguments at ${path}: ${message}. Call describe_tool("${ref}") for the schema.`,
  needsConnection: (name: string, publicUrl: string) =>
    `${name} is not connected. Ask the user to open ${publicUrl}/connections and connect ${name}.`,
  expired: (name: string, publicUrl: string) =>
    `${name} connection expired. Ask the user to reconnect at ${publicUrl}/connections.`,
  rejected: (name: string) =>
    `${name} rejected the gateway credentials. Ask the administrator to check the server key.`,
  quota: (name: string, resetAt: Date) =>
    `Daily limit for ${name} reached. Resets at ${resetAt.toISOString()}.`,
  timeout: (name: string, timeoutMs: number) =>
    `${name} did not respond in ${round(timeoutMs / 1000)} s.`,
  transport: (name: string) => `${name} is unavailable.`,
  /** Upstream answered with a JSON-RPC error: its code and (cut) message, as upstream wrote it. */
  upstreamRejected: (name: string, rejection: UpstreamRejection) => {
    const message = rejection.message.trim();
    const head = `${name} rejected the call (JSON-RPC ${rejection.code})`;
    return message === '' ? `${head}.` : `${head}: ${cut(message)}`;
  },
  tooLarge: (name: string, maxBytes: number) =>
    `${name} returned a response larger than ${round(maxBytes / MIB)} MiB.`,
};

type Ended = { outcome: CallOutcome; result: ToolCallResult };
type Denied = { outcome: CallOutcome; text: string };

const failed = ({ outcome, text }: Denied): Ended => ({
  outcome,
  result: { content: [{ type: 'text', text }], isError: true },
});

/**
 * `call_tool`: checks, in order, that the tool is callable by this person right now, the
 * arguments, the credentials (before the limit, so a missing connection spends nothing) and the
 * daily limit; then calls upstream once (an OAuth 401 gets one refresh and one retry). The
 * upstream result is returned as is. Every call is logged — without arguments or results.
 */
export async function callTool(
  deps: GatewayDeps,
  cmd: { principal: Principal; ref: string; arguments: Record<string, unknown> },
): Promise<ToolCallResult> {
  const startedAt = deps.clock.now();
  const parsed = ToolRef.parse(cmd.ref);
  // An unexpected failure (not an UpstreamError) is rethrown and logged as transport_error.
  let outcome: CallOutcome = 'transport_error';
  try {
    const ended = await run(deps, cmd);
    outcome = ended.outcome;
    return ended.result;
  } finally {
    const entry = {
      userId: cmd.principal.userId,
      tokenId: cmd.principal.tokenId ?? null,
      server: bounded(parsed.ok ? parsed.value.server : ''),
      tool: bounded(parsed.ok ? parsed.value.tool : cmd.ref),
      startedAt,
      durationMs: deps.clock.now().getTime() - startedAt.getTime(),
      outcome,
    };
    try {
      await deps.callLog.record(entry);
    } catch (error) {
      // The upstream call has happened: a lost log entry must not change the agent's answer.
      deps.logger.error(
        { err: errorFields(error), server: entry.server, tool: entry.tool, outcome },
        'gateway: call log record failed',
      );
    }
  }
}

async function run(
  deps: GatewayDeps,
  cmd: { principal: Principal; ref: string; arguments: Record<string, unknown> },
): Promise<Ended> {
  const { principal, ref } = cmd;
  const notFound = failed({ outcome: 'denied:not_found', text: CALL_TOOL_TEXT.notFound(ref) });
  const tool = await deps.catalog.toolFor(principal, ref);
  if (!tool) return notFound;
  const server = await deps.catalog.serverFor(tool.server);
  if (!server) return notFound;

  const check = deps.validator.validate(ref, tool.inputSchema, cmd.arguments);
  if (!check.ok) {
    const text = CALL_TOOL_TEXT.invalidArguments(ref, check.path, check.message);
    return failed({ outcome: 'denied:invalid_arguments', text });
  }

  const secret = await secretFor(deps, server, principal.userId, false);
  if (!secret.ok) return failed(secret.error);

  if (server.dailyLimit !== null) {
    const { day } = QuotaDay.of(deps.clock.now());
    const quota = await deps.quota.tryConsume({
      userId: principal.userId,
      server: server.slug,
      day,
      limit: server.dailyLimit,
    });
    if (!quota.ok) {
      return failed({
        outcome: 'denied:quota',
        text: CALL_TOOL_TEXT.quota(server.name, quota.resetAt),
      });
    }
  }

  return send(deps, server, principal.userId, tool.name, cmd.arguments, secret.value);
}

/** The upstream secret: none, the server's own key, or the person's token. */
async function secretFor(
  deps: GatewayDeps,
  server: ServerCallTarget,
  userId: string,
  forceRefresh: boolean,
): Promise<Result<string, Denied>> {
  const source = AuthMode.credentialSource(server.auth);
  if (source === 'none') return ok('');
  if (source === 'server_secret') {
    const secret = await deps.catalog.serverSecret(server.slug);
    return secret === null ? err(rejected(server)) : ok(secret);
  }
  const r = await deps.credentials.resolve({ userId, server: server.slug, forceRefresh });
  if (r.ok) return r;
  if (r.error === 'needs_connection') {
    return err({
      outcome: CallOutcome.ofCredentialFailure(r.error),
      text: CALL_TOOL_TEXT.needsConnection(server.name, deps.publicUrl),
    });
  }
  if (r.error === 'expired') return err(expired(deps, server));
  return err({
    outcome: CallOutcome.ofCredentialFailure(r.error),
    text: CALL_TOOL_TEXT.transport(server.name),
  });
}

const rejected = (server: ServerCallTarget): Denied => ({
  outcome: 'transport_error',
  text: CALL_TOOL_TEXT.rejected(server.name),
});

const expired = (deps: GatewayDeps, server: ServerCallTarget): Denied => ({
  outcome: 'denied:expired',
  text: CALL_TOOL_TEXT.expired(server.name, deps.publicUrl),
});

async function send(
  deps: GatewayDeps,
  server: ServerCallTarget,
  userId: string,
  tool: string,
  args: Record<string, unknown>,
  secret: string,
): Promise<Ended> {
  const attempt = async (s: string): Promise<Ended | 'unauthorized'> => {
    const target = {
      url: server.url,
      headers: AuthMode.headerFor(server.auth, s),
      timeoutMs: server.timeoutMs,
      maxResponseBytes: deps.maxResponseBytes,
    };
    try {
      const result = await deps.upstream.callTool(target, tool, args);
      return { outcome: CallOutcome.ofResult(result), result };
    } catch (error) {
      if (!(error instanceof UpstreamError)) throw error;
      if (error.kind === 'unauthorized') return 'unauthorized';
      return failed(upstreamFailure(deps, server, error.kind, error.rejection));
    }
  };

  const first = await attempt(secret);
  if (first !== 'unauthorized') return first;
  const who = { userId, server: server.slug };
  const action = onUpstreamUnauthorized(server.auth.kind);
  if (action === 'reject') return failed(rejected(server));
  if (action === 'refresh_and_retry') {
    const fresh = await secretFor(deps, server, userId, true);
    if (!fresh.ok) return failed(fresh.error);
    const second = await attempt(fresh.value);
    if (second !== 'unauthorized') return second;
    // A just-refreshed token is rejected too: only a new connection can help.
  }
  await deps.credentials.markExpired(who);
  return failed(expired(deps, server));
}

function upstreamFailure(
  deps: GatewayDeps,
  server: ServerCallTarget,
  kind: Exclude<UpstreamErrorKind, 'unauthorized'>,
  rejection: UpstreamRejection | null,
): Denied {
  const outcome = CallOutcome.ofUpstreamFailure(kind);
  if (kind === 'rejected') {
    const text = CALL_TOOL_TEXT.upstreamRejected(
      server.name,
      rejection ?? { code: 0, message: '' },
    );
    return { outcome, text };
  }
  if (kind === 'timeout') {
    return { outcome, text: CALL_TOOL_TEXT.timeout(server.name, server.timeoutMs) };
  }
  if (kind === 'too_large') {
    return { outcome, text: CALL_TOOL_TEXT.tooLarge(server.name, deps.maxResponseBytes) };
  }
  return { outcome, text: CALL_TOOL_TEXT.transport(server.name) };
}
