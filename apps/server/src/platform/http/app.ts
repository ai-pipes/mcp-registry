import { randomUUID } from 'node:crypto';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { type Context, Hono } from 'hono';
import { HTTPException } from 'hono/http-exception';
import { serverRoutes, toolRoutes } from '../../modules/catalog/index.ts';
import { connectionRoutes } from '../../modules/connections/index.ts';
import { callRoutes } from '../../modules/gateway/index.ts';
import { identityRoutes } from '../../modules/identity/index.ts';
import { DomainError, errorFields } from '../../shared-kernel/index.ts';
import type { Container } from '../container.ts';
import { mcpRoutes } from '../mcp/server.ts';
import type { AppEnv } from './env.ts';
import { auditRoutes } from './audit-route.ts';
import { domainErrorResponse, errorResponse } from './errors.ts';
import { spaFallback, staticFiles } from './spa.ts';
import { clientIp, csrf, defaultBodyLimit, securityHeaders, socketAddress } from './security.ts';

const DEFAULT_WEB_ROOT = fileURLToPath(new URL('../../../../web/dist', import.meta.url));
const HEALTHZ_TIMEOUT_MS = 1500;

export type HttpAppOptions = {
  /** Peer address of the connection; defaults to the Node socket. Tests inject one. */
  remoteAddress?: (c: Context) => string;
  /** Built UI; defaults to apps/web/dist. Not served when the directory is missing. */
  webRoot?: string;
};

export type HttpApp = {
  app: Hono<AppEnv>;
  /** Aborts in-flight MCP exchanges; call on shutdown after the HTTP server stops accepting. */
  close: () => Promise<void>;
};

export function createHttpApp(c: Container, opts: HttpAppOptions = {}): HttpApp {
  const app = new Hono<AppEnv>();
  const { logger, config } = c;

  // 1. request id + access log (method, path, status, duration; never headers or bodies)
  app.use(async (ctx, next) => {
    const requestId = randomUUID();
    const started = performance.now();
    ctx.set('requestId', requestId);
    ctx.header('X-Request-Id', requestId);
    await next();
    logger.info(
      {
        reqId: requestId,
        method: ctx.req.method,
        path: ctx.req.path,
        status: ctx.res.status,
        ms: Math.round(performance.now() - started),
        ip: ctx.get('clientIp'),
      },
      'request',
    );
  });
  // 2. security headers  3. body limit  4. client IP  5. CSRF on mutating /api requests
  app.use(securityHeaders());
  // `/mcp` has its own, larger limit (MCP_MAX_BODY_BYTES), applied on its route.
  const restBodyLimit = defaultBodyLimit();
  app.use(async (ctx, next) =>
    ctx.req.path === '/mcp' || ctx.req.path.startsWith('/mcp/') ? next() : restBodyLimit(ctx, next),
  );
  app.use(
    clientIp({
      trustProxy: config.trustProxy,
      trustProxyHops: config.trustProxyHops,
      remoteAddress: opts.remoteAddress ?? socketAddress,
    }),
  );
  app.use(csrf(config.publicUrl));

  app.onError((error, ctx) => {
    if (error instanceof DomainError) return domainErrorResponse(ctx, error);
    if (error instanceof HTTPException) return error.getResponse();
    // The message may carry user input or secrets: log only the name and the stack frames.
    logger.error({ reqId: ctx.get('requestId'), err: errorFields(error) }, 'unhandled error');
    return ctx.json({ error: { code: 'internal', message: 'Internal error' } }, 500);
  });
  app.notFound((ctx) =>
    ctx.req.path.startsWith('/api/')
      ? errorResponse(ctx, 'not_found', 'not found')
      : ctx.text('Not found', 404),
  );

  // Liveness = the database only. Upstream servers and the embedding model are deliberately
  // not checked: without them the gateway degrades but stays healthy.
  app.get('/healthz', async (ctx) => {
    let timer: NodeJS.Timeout | undefined;
    try {
      const timeout = new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error('healthz timeout')), HEALTHZ_TIMEOUT_MS);
      });
      await Promise.race([c.sql`select 1`, timeout]);
      return ctx.text('ok', 200);
    } catch {
      return ctx.text('fail', 503);
    } finally {
      clearTimeout(timer);
    }
  });

  // Modules mount their routes here: /api/** (session cookie only) and /mcp (Bearer sk_…).
  // The message may carry user input or secrets: only the name and the stack frames are logged.
  const logError = (error: unknown, message: string) => {
    logger.error({ err: errorFields(error) }, message);
  };
  app.route('/api', identityRoutes(c.identity, { cookieSecure: config.cookieSecure, logError }));
  app.route('/api/connections', connectionRoutes(c.connections, c.identity, { logError }));
  app.route('/api', toolRoutes(c.catalog, c.identity));
  app.route('/api/servers', serverRoutes(c.catalogSync, c.identity));
  app.route('/api/calls', callRoutes(c.gateway, c.identity));
  app.route('/api/audit', auditRoutes(c.sql, c.identity));
  const mcp = mcpRoutes(c);
  app.route('/mcp', mcp.routes);

  const webRoot = opts.webRoot ?? DEFAULT_WEB_ROOT;
  if (existsSync(webRoot)) {
    app.use('/*', staticFiles(webRoot));
    app.use('/*', spaFallback(webRoot));
  }
  return { app, close: mcp.close };
}
