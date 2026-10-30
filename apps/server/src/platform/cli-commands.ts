import { readFile } from 'node:fs/promises';
import { parseArgs } from 'node:util';
import { ServerInput } from '@mcp/contracts';
import {
  AuthMode,
  type ResealSummary,
  registerServer,
  resealServerSecrets,
  syncServer,
  toAuthMode,
} from '../modules/catalog/index.ts';
import { resealConnections } from '../modules/connections/index.ts';
import { actAs, createToken, type Principal, provisionUser } from '../modules/identity/index.ts';
import { reindexAll } from '../modules/search/index.ts';
import { DomainError } from '../shared-kernel/index.ts';
import type { Container } from './container.ts';
import { AesGcmSecretBox } from './crypto/secret-box.ts';

export type CliIo = {
  /** The whole of stdin (a pipe), or one line typed without echo on a terminal. */
  readStdin(): Promise<string>;
  /** An environment variable, for `--secret-env`. */
  env(name: string): string | undefined;
  out(line: string): void;
  err(line: string): void;
};

const USAGE = `Usage: node dist/cli.js <command>

  create-user --sub <sub> --name <name> [--admin]
      Create (or update) a person as their first SSO login would: member, or admin with
      --admin. Idempotent by sub; without --admin an existing admin becomes a member.
      For the smoke test and emergency access.
  create-token <sub> --name <name>
      Create an API token for <sub>; prints the token and the claude mcp add command.
  create-server --json <file> --as <admin-sub> [--secret-env <VAR>]
      Create a server from a ServerInput JSON file and synchronise it at once. The secret
      (shared key or OAuth client secret) is read from the variable <VAR> or from stdin,
      never from the command line, and is never printed.
  sync <slug> [--as <admin-sub>]
      Synchronise the server's tools; without --as, as the scheduler does.
  reindex
      Rebuild the search index for every server.
  rotate-secrets-key
      Re-seal every stored secret (server secrets and people's tokens) from SECRETS_KEY_OLD
      to SECRETS_KEY. Safe to repeat.`;

/** Bad invocation: printed with the usage, exit code 2. */
class UsageError extends Error {}

/** The operation failed for a reason the operator can fix: exit code 1, no usage. */
class CommandError extends Error {}

type Command = (args: string[], c: Container, io: CliIo) => Promise<number>;

type Options = Record<string, { type: 'string' | 'boolean' }>;

function parse(args: string[], options: Options, positionals: number) {
  let parsed;
  try {
    parsed = parseArgs({ args, options, allowPositionals: true, strict: true });
  } catch (error) {
    throw new UsageError(error instanceof Error ? error.message : String(error));
  }
  if (parsed.positionals.length !== positionals) {
    throw new UsageError(`expected ${positionals} argument(s), got ${parsed.positionals.length}`);
  }
  return { values: parsed.values, positionals: parsed.positionals };
}

function required(values: Record<string, unknown>, name: string): string {
  const v = values[name];
  if (typeof v !== 'string' || v === '') throw new UsageError(`--${name} is required`);
  return v;
}

function optional(values: Record<string, unknown>, name: string): string | undefined {
  const v = values[name];
  return typeof v === 'string' ? v : undefined;
}

/** Strips one trailing newline (what `echo` or a heredoc adds), nothing else. */
const stripNewline = (s: string): string => s.replace(/\r?\n$/, '');

async function principalFor(c: Container, sub: string): Promise<Principal> {
  const r = await actAs(c.identity, sub);
  if (!r.ok) throw r.error;
  return r.value;
}

/** The command that connects Claude Code to this gateway with `token`. */
const connectCommand = (publicUrl: string, token: string): string =>
  `claude mcp add --transport http mcp-registry ${new URL('/mcp', publicUrl).toString()} ` +
  `--header "Authorization: Bearer ${token}"`;

const createUserCommand: Command = async (args, c, io) => {
  const { values } = parse(
    args,
    { sub: { type: 'string' }, name: { type: 'string' }, admin: { type: 'boolean' } },
    0,
  );
  const sub = required(values, 'sub');
  const name = required(values, 'name');
  const r = await provisionUser(c.identity, { sub, name, admin: values.admin === true });
  if (!r.ok) throw r.error;
  const verb = r.value.created ? 'created' : 'updated';
  io.out(`${verb} ${r.value.role} "${name.trim()}" (sub ${sub.trim()}, id ${r.value.id})`);
  return 0;
};

const createTokenCommand: Command = async (args, c, io) => {
  const { values, positionals } = parse(args, { name: { type: 'string' } }, 1);
  const name = required(values, 'name');
  const principal = await principalFor(c, positionals[0] ?? '');
  const t = await createToken(c.identity, { principal, name });
  io.out(t.token);
  io.out(connectCommand(c.config.publicUrl, t.token));
  io.err('The token is shown only once; store it now.');
  return 0;
};

async function readServerInput(file: string): Promise<ServerInput> {
  let text: string;
  try {
    text = await readFile(file, 'utf8');
  } catch {
    throw new CommandError(`cannot read ${file}`);
  }
  let json: unknown;
  try {
    json = JSON.parse(text);
  } catch {
    throw new CommandError(`${file} is not valid JSON`);
  }
  const parsed = ServerInput.safeParse(json);
  if (!parsed.success) {
    const issues = parsed.error.issues.map((i) => `${i.path.join('.') || '(root)'}: ${i.message}`);
    throw new CommandError(`${file} is not a valid server:\n  ${issues.join('\n  ')}`);
  }
  return parsed.data;
}

/** The server secret: from `--secret-env`, else from stdin when the mode holds one. */
async function readSecret(
  io: CliIo,
  secretEnv: string | undefined,
  auth: AuthMode,
): Promise<string | null> {
  if (secretEnv !== undefined) {
    const value = io.env(secretEnv);
    if (value === undefined || value === '') throw new UsageError(`${secretEnv} is unset or empty`);
    return value;
  }
  if (!AuthMode.holdsSecret(auth)) return null;
  const typed = stripNewline(await io.readStdin());
  return typed === '' ? null : typed;
}

const createServerCommand: Command = async (args, c, io) => {
  const { values } = parse(
    args,
    { json: { type: 'string' }, as: { type: 'string' }, 'secret-env': { type: 'string' } },
    0,
  );
  const file = required(values, 'json');
  const sub = required(values, 'as');
  const input = await readServerInput(file);
  const principal = await principalFor(c, sub);
  const auth = toAuthMode(input.auth);
  const secret = await readSecret(io, optional(values, 'secret-env'), auth);
  const r = await registerServer(c.catalogSync, { principal, ...input, auth, secret });
  if (!r.ok) throw r.error;
  const s = r.value;
  const mode = s.secretSet ? `${s.auth.kind}, secret set` : s.auth.kind;
  const sync =
    s.lastSyncError === null ? `${s.toolCount} tool(s)` : `sync failed: ${s.lastSyncError}`;
  io.out(`created server "${s.slug}" (${mode}); ${sync}`);
  return 0;
};

const syncCommand: Command = async (args, c, io) => {
  const { values, positionals } = parse(args, { as: { type: 'string' } }, 1);
  const sub = optional(values, 'as');
  const principal = sub === undefined ? null : await principalFor(c, sub);
  const slug = positionals[0] ?? '';
  const r = await syncServer(c.catalogSync, { principal, slug });
  if (!r.ok) throw r.error;
  const { added, changed, removed } = r.value;
  io.out(`synced "${slug}": ${added} added, ${changed} changed, ${removed} removed`);
  return 0;
};

const reindexCommand: Command = async (args, c, io) => {
  parse(args, {}, 0);
  const count = await reindexAll(c.search);
  io.out(`reindexed ${count} server(s)`);
  return 0;
};

const resealLine = (what: string, s: ResealSummary) =>
  `${what}: ${s.resealed} re-sealed, ${s.current} already under the new key`;

const rotateSecretsKeyCommand: Command = async (args, c, io) => {
  parse(args, {}, 0);
  const old = c.config.secretsKeyOld;
  if (!old) {
    throw new CommandError(
      'SECRETS_KEY_OLD is not set: put the previous key there, the new one in SECRETS_KEY',
    );
  }
  const keys = { from: new AesGcmSecretBox(old), to: new AesGcmSecretBox(c.config.secretsKey) };
  const servers = await resealServerSecrets(c.catalog, keys);
  const connections = await resealConnections(c.connections, keys);
  io.out(resealLine('server secrets', servers));
  io.out(resealLine('connections', connections));
  await c.audit.record({
    actorId: null,
    action: 'secrets.rotate',
    meta: {
      via: 'cli',
      servers: {
        resealed: servers.resealed,
        current: servers.current,
        failed: servers.failed.length,
      },
      connections: {
        resealed: connections.resealed,
        current: connections.current,
        failed: connections.failed.length,
      },
    },
  });
  const failed = [
    ...servers.failed.map((slug) => `server ${slug}`),
    ...connections.failed.map((key) => `connection ${key}`),
  ];
  if (failed.length === 0) return 0;
  io.err('error: neither key opens (left as they were):');
  for (const f of failed) io.err(`  ${f}`);
  return 1;
};

const COMMANDS: Record<string, Command> = {
  'create-user': createUserCommand,
  'create-token': createTokenCommand,
  'create-server': createServerCommand,
  sync: syncCommand,
  reindex: reindexCommand,
  'rotate-secrets-key': rotateSecretsKeyCommand,
};

/**
 * Runs one CLI command against an already built container, through the modules' public use
 * cases. Returns the exit code: 0 success, 1 the operation failed, 2 bad invocation (usage
 * printed to `err`). Secrets are never printed.
 */
export async function runCli(argv: string[], c: Container, io: CliIo): Promise<number> {
  const [name = '', ...args] = argv;
  const command = COMMANDS[name];
  try {
    if (!command) {
      throw new UsageError(name === '' ? 'no command given' : `unknown command "${name}"`);
    }
    return await command(args, c, io);
  } catch (error) {
    if (error instanceof UsageError) {
      io.err(`error: ${error.message}`);
      io.err(USAGE);
      return 2;
    }
    if (error instanceof CommandError) {
      io.err(`error: ${error.message}`);
      return 1;
    }
    if (error instanceof DomainError) {
      io.err(`error: ${error.code}: ${error.message}`);
      return 1;
    }
    throw error;
  }
}
