// Server entry point (`node dist/main.js`): migrate, start the outbox worker and the scheduler
// (catalog synchronisation), and serve HTTP on 0.0.0.0:$PORT.
import { fileURLToPath } from 'node:url';
import { serve } from '@hono/node-server';
import { indexMissingEmbeddings, searchEventHandlers } from '../modules/search/index.ts';
import { OpenidProvider } from '../modules/identity/index.ts';
import { errorFields } from '../shared-kernel/index.ts';
import { loadConfigOrExit } from './config.ts';
import { buildContainer } from './container.ts';
import { createDb } from './db.ts';
import { createLogger } from './logger.ts';
import { createHttpApp } from './http/app.ts';
import { findMigrationsDir, migrate } from './migrate.ts';
import { startOutboxWorker } from './outbox-worker.ts';
import { startScheduler } from './scheduler.ts';

const OUTBOX_INTERVAL_MS = 1000;
const EMBED_SWEEP_INTERVAL_MS = 5 * 60_000;
const EMBED_SWEEP_LIMIT = 20;
const SHUTDOWN_TIMEOUT_MS = 10_000;
// This file only runs bundled, as apps/server/dist/main.js; the built UI is apps/web/dist.
const WEB_ROOT = fileURLToPath(new URL('../../web/dist', import.meta.url));

const config = loadConfigOrExit(process.env);
const sql = createDb(config.databaseUrl);
const logger = createLogger(config.logLevel);
// Discovered lazily, on the first sign-in: the gateway (/mcp, tokens) works while it is down.
const oidc = new OpenidProvider({
  issuer: config.oidc.issuer,
  clientId: config.oidc.clientId,
  clientSecret: config.oidc.clientSecret,
  redirectUri: new URL('/api/auth/callback', config.publicUrl).toString(),
  logger,
});
const container = buildContainer(config, sql, { oidc, logger });
if (config.secretsKeyOld) {
  // The previous key still opens secrets while it is set: finish the rotation.
  logger.warn(
    'SECRETS_KEY_OLD is set: key rotation in progress. Run `node dist/cli.js rotate-secrets-key`, then remove SECRETS_KEY_OLD and restart',
  );
}

const applied = await migrate(sql, findMigrationsDir(import.meta.url));
logger.info({ applied }, 'migrations applied');

// Modules register their event handlers here (type → handler) as they appear.
const worker = startOutboxWorker({
  sql,
  handlers: { ...searchEventHandlers(container.search) },
  intervalMs: OUTBOX_INTERVAL_MS,
  logger,
  // Fills in embeddings that an embedder outage left empty.
  sweep: {
    everyMs: EMBED_SWEEP_INTERVAL_MS,
    run: async () => {
      await indexMissingEmbeddings(container.search, { limit: EMBED_SWEEP_LIMIT });
    },
  },
});
const scheduler = startScheduler(container);

const http = createHttpApp(container, { webRoot: WEB_ROOT });
const server = serve({ fetch: http.app.fetch, hostname: '0.0.0.0', port: config.port }, (info) =>
  logger.info({ port: info.port }, 'listening'),
);

let shuttingDown = false;
async function shutdown(signal: string): Promise<void> {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info({ signal }, 'shutting down');
  setTimeout(() => {
    logger.error('shutdown timed out');
    process.exit(1);
  }, SHUTDOWN_TIMEOUT_MS).unref();
  // Stop accepting first; in-flight requests drain while the rest shuts down.
  const drained = new Promise<void>((resolve) => server.close(() => resolve()));
  await Promise.all([worker.stop(), scheduler.stop()]);
  await http.close();
  await drained;
  await sql.end({ timeout: 5 });
  logger.info('stopped');
  process.exit(0);
}
for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    shutdown(signal).catch((error: unknown) => {
      logger.error({ err: errorFields(error) }, 'shutdown failed');
      process.exit(1);
    });
  });
}
