import {
  type AuthInfo,
  createMcpHandler,
  hostHeaderValidationResponse,
  McpServer,
} from '@modelcontextprotocol/server';
import { Hono, type MiddlewareHandler } from 'hono';
import {
  type IdentityEnv,
  type Principal,
  requireBearerToken,
} from '../../modules/identity/index.ts';
import { errorFields } from '../../shared-kernel/index.ts';
import type { Container } from '../container.ts';
import { errorResponse } from '../http/errors.ts';
import { defaultBodyLimit } from '../http/security.ts';
import { SERVER_INSTRUCTIONS } from './instructions.ts';
import { registerTools } from './tools.ts';

const SERVER_VERSION = '0.0.0';

/**
 * One MCP server per request, for one authenticated person: every tool answers for them only.
 * `listChanged: false` — the four tools never change, so `tools/list_changed` is never sent.
 */
function buildMcpServer(c: Container, principal: Principal): McpServer {
  const server = new McpServer(
    { name: 'mcp-registry', version: SERVER_VERSION },
    { instructions: SERVER_INSTRUCTIONS, capabilities: { tools: { listChanged: false } } },
  );
  registerTools(server, c, principal);
  return server;
}

function isPrincipal(v: unknown): v is Principal {
  return (
    typeof v === 'object' &&
    v !== null &&
    'userId' in v &&
    typeof v.userId === 'string' &&
    'via' in v &&
    v.via === 'token'
  );
}

/** The principal set by the Bearer check; throws if the handler was reached without one. */
function principalOf(authInfo: AuthInfo | undefined): Principal {
  const principal = authInfo?.extra?.['principal'];
  if (!isPrincipal(principal)) throw new Error('MCP request without an authenticated principal');
  return principal;
}

/** Hostnames accepted in `Host`: the configured ones, the public URL's, and loopback. */
function mcpAllowedHosts(config: Container['config']): string[] {
  const hosts = [
    ...config.allowedHosts,
    new URL(config.publicUrl).hostname,
    'localhost',
    '127.0.0.1',
  ];
  return [...new Set(hosts.map((h) => h.toLowerCase()))];
}

/** DNS rebinding guard: a `Host` outside the allowlist gets the SDK's 403. */
function hostCheck(allowed: string[]): MiddlewareHandler {
  return async (ctx, next) => hostHeaderValidationResponse(ctx.req.raw, allowed) ?? next();
}

/** A browser page from another origin cannot use /mcp; clients without `Origin` (CLIs) can. */
function originCheck(publicUrl: string): MiddlewareHandler {
  const expected = new URL(publicUrl).origin;
  return async (ctx, next) => {
    const origin = ctx.req.header('origin');
    if (origin !== undefined && origin !== expected) {
      return errorResponse(ctx, 'forbidden', 'origin not allowed');
    }
    return next();
  };
}

/**
 * `/mcp` (Streamable HTTP). Order: Host → Origin → Bearer `sk_…` (identity) → MCP handler.
 * Body size is capped by its own limit (MCP_MAX_BODY_BYTES, default 10 MiB; the rest of the app
 * keeps 1 MiB), so large tool arguments (files as base64) reach `call_tool`; `/mcp` is outside `/api`, so neither the session
 * middleware nor the CSRF check applies. The token is never passed on or logged: the handler
 * gets `'<redacted>'` and the resolved principal.
 */
export function mcpRoutes(c: Container): {
  routes: Hono<IdentityEnv>;
  /** Aborts in-flight MCP exchanges; call on shutdown after the HTTP server stops accepting. */
  close: () => Promise<void>;
} {
  const handler = createMcpHandler((ctx) => buildMcpServer(c, principalOf(ctx.authInfo)), {
    onerror: (error) => c.logger.warn({ err: errorFields(error) }, 'mcp error'),
    // The SDK reads the body itself with its own cap (4 MiB by default): the same limit.
    maxRequestBodySize: c.config.mcpMaxBodyBytes,
  });
  const routes = new Hono<IdentityEnv>().all(
    '/',
    defaultBodyLimit(c.config.mcpMaxBodyBytes),
    hostCheck(mcpAllowedHosts(c.config)),
    originCheck(c.config.publicUrl),
    requireBearerToken(c.identity),
    (ctx) => {
      const principal = ctx.get('principal');
      return handler.fetch(ctx.req.raw, {
        authInfo: {
          token: '<redacted>',
          clientId: principal.userId,
          scopes: [],
          extra: { principal },
        },
      });
    },
  );
  return { routes, close: () => handler.close() };
}
