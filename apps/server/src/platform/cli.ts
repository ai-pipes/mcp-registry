// Operator CLI: `node dist/cli.js <command>` (see USAGE in cli-commands.ts). Runs migrations
// first, so it also works against a fresh database. Logs go to stderr; command output to stdout.
// Needs the same environment as the server (it reads the same config); OIDC is not contacted.
import { runCli, runVersionFlag } from './cli-commands.ts';
import { readSecretInput } from './cli-input.ts';
import { loadConfigOrExit } from './config.ts';
import { buildContainer } from './container.ts';
import { createDb } from './db.ts';
import { createLogger } from './logger.ts';
import { findMigrationsDir, migrate, NewerDatabaseError } from './migrate.ts';

// `--version` needs no configuration and no database.
const versionExit = runVersionFlag(process.argv.slice(2), {
  out: (line) => process.stdout.write(`${line}\n`),
});
if (versionExit !== null) process.exit(versionExit);

const config = loadConfigOrExit(process.env);
const sql = createDb(
  config.databaseUrl,
  config.databasePassword === undefined ? {} : { password: config.databasePassword },
);
const logger = createLogger(config.logLevel === 'info' ? 'warn' : config.logLevel, {
  write: (line: string) => process.stderr.write(line),
});
try {
  await migrate(sql, findMigrationsDir(import.meta.url));
  const container = buildContainer(config, sql, { logger });
  process.exitCode = await runCli(process.argv.slice(2), container, {
    readStdin: () => readSecretInput(process.stdin, process.stderr),
    env: (name) => process.env[name],
    out: (line) => process.stdout.write(`${line}\n`),
    err: (line) => process.stderr.write(`${line}\n`),
  });
} catch (error) {
  // A database from a newer version: a plain message and exit 1, not a stack trace.
  if (!(error instanceof NewerDatabaseError)) throw error;
  process.stderr.write(`error: ${error.message}\n`);
  process.exitCode = 1;
} finally {
  await sql.end({ timeout: 5 });
}
// The local embedder's native runtime can keep the event loop alive after `reindex`.
process.exit();
