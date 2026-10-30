import { z } from 'zod';
// The calibrated cosine-similarity floor lives in the search module, where the
// real-model test checks it: one constant, so recalibrating it changes production too.
import { DEFAULT_MIN_SIMILARITY } from '../modules/search/index.ts';

const SECRETS_KEY_BYTES = 32;
const DEFAULT_UPSTREAM_MAX_RESPONSE_BYTES = 20 * 1024 * 1024;
/** `/mcp` request bodies: tool arguments can carry files (base64), well above the REST 1 MiB. */
const DEFAULT_MCP_MAX_BODY_BYTES = 10 * 1024 * 1024;

const bool = (fallback: boolean) =>
  z
    .enum(['true', 'false'])
    .optional()
    .transform((v) => (v === undefined ? fallback : v === 'true'));

/** 32 bytes in canonical base64. The message never contains the value. */
const secretsKey = z.string().transform((value, ctx): Uint8Array => {
  const bytes = Buffer.from(value, 'base64');
  if (bytes.toString('base64') !== value || bytes.length !== SECRETS_KEY_BYTES) {
    ctx.addIssue({
      code: 'custom',
      message: `must be base64 of exactly ${SECRETS_KEY_BYTES} bytes (openssl rand -base64 32)`,
    });
    return z.NEVER;
  }
  return new Uint8Array(bytes);
});

const positiveInt = (fallback: number) => z.coerce.number().int().min(1).default(fallback);
/** A year: a longer synchronisation period is a typo. */
const MAX_SYNC_INTERVAL_HOURS = 24 * 365;

const schema = z.object({
  PORT: z.coerce.number().int().min(1).max(65535).default(3000),
  DATABASE_URL: z.string().min(1),
  PUBLIC_URL: z.url(),
  ALLOWED_HOSTS: z
    .string()
    .default('')
    .transform((v) =>
      v
        .split(',')
        .map((h) => h.trim())
        .filter((h) => h.length > 0),
    ),
  TRUST_PROXY: bool(false),
  TRUST_PROXY_HOPS: z.coerce.number().int().min(1).max(5).default(1),
  COOKIE_SECURE: bool(true),
  SECRETS_KEY: secretsKey,
  SECRETS_KEY_OLD: secretsKey.optional(),
  OIDC_ISSUER: z.url(),
  OIDC_CLIENT_ID: z.string().min(1),
  OIDC_CLIENT_SECRET: z.string().min(1),
  OIDC_LOGIN_GROUP: z.string().min(1).default('mcp-users'),
  OIDC_ADMIN_GROUP: z.string().min(1).default('mcp-admins'),
  SYNC_INTERVAL_HOURS: z.coerce.number().int().min(1).max(MAX_SYNC_INTERVAL_HOURS).default(6),
  CALL_LOG_RETENTION_DAYS: positiveInt(90),
  UPSTREAM_MAX_RESPONSE_BYTES: positiveInt(DEFAULT_UPSTREAM_MAX_RESPONSE_BYTES),
  MCP_MAX_BODY_BYTES: positiveInt(DEFAULT_MCP_MAX_BODY_BYTES),
  EMBEDDER: z.enum(['local', 'openai-compatible']).default('local'),
  EMBEDDER_URL: z.url().optional(),
  EMBEDDER_MODEL: z.string().min(1).optional(),
  MODEL_DIR: z.string().default('/app/models'),
  SEARCH_MIN_SIMILARITY: z.coerce.number().min(0).max(1).default(DEFAULT_MIN_SIMILARITY),
  LOG_LEVEL: z.enum(['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent']).default('info'),
});

/** Invalid environment: the message names each bad variable, one per line (never a value). */
export class ConfigError extends Error {
  constructor(issues: readonly z.core.$ZodIssue[]) {
    const lines = issues.map((i) => `  ${i.path.map(String).join('.') || '(env)'}: ${i.message}`);
    super(`Invalid configuration:\n${lines.join('\n')}`);
    this.name = 'ConfigError';
  }
}

interface OidcConfig {
  issuer: string;
  clientId: string;
  clientSecret: string;
  /** Members of this group (or of `adminGroup`) may sign in. */
  loginGroup: string;
  /** Members of this group are admins. */
  adminGroup: string;
}

export interface Config {
  port: number;
  databaseUrl: string;
  publicUrl: string;
  allowedHosts: string[];
  trustProxy: boolean;
  /** With trustProxy: the client is the hop this many entries from the right of X-Forwarded-For. */
  trustProxyHops: number;
  cookieSecure: boolean;
  /** AES-256-GCM key for upstream secrets and people's tokens (32 bytes). */
  secretsKey: Uint8Array;
  /**
   * The previous key while SECRETS_KEY rotates: still opens what `rotate-secrets-key` has not
   * re-sealed yet, and is the key that command re-seals from.
   */
  secretsKeyOld?: Uint8Array;
  oidc: OidcConfig;
  /** Catalog synchronisation period. */
  syncIntervalHours: number;
  callLogRetentionDays: number;
  /** Upstream `tools/call` answers above this size are refused. */
  upstreamMaxResponseBytes: number;
  /** Request body limit of `/mcp` (MCP_MAX_BODY_BYTES); the rest of the app keeps 1 MiB. */
  mcpMaxBodyBytes: number;
  embedder: 'local' | 'openai-compatible';
  embedderUrl?: string;
  embedderModel?: string;
  modelDir: string;
  /** Cosine-similarity floor for vector search candidates. */
  searchMinSimilarity: number;
  logLevel: string;
}

/** loadConfig for entry points: on bad env print the ConfigError message (no stack) and exit 1. */
export function loadConfigOrExit(env: NodeJS.ProcessEnv): Config {
  try {
    return loadConfig(env);
  } catch (error) {
    if (!(error instanceof ConfigError)) throw error;
    process.stderr.write(`${error.message}\n`);
    return process.exit(1);
  }
}

export function loadConfig(env: NodeJS.ProcessEnv): Config {
  // An empty value (`KEY=` in .env) means "unset", so defaults and optionality apply to it.
  const set = Object.fromEntries(Object.entries(env).filter(([, v]) => v !== ''));
  const parsed = schema
    .refine(
      (e) =>
        e.SECRETS_KEY_OLD === undefined ||
        Buffer.compare(Buffer.from(e.SECRETS_KEY_OLD), Buffer.from(e.SECRETS_KEY)) !== 0,
      { path: ['SECRETS_KEY_OLD'], message: 'must differ from SECRETS_KEY' },
    )
    .safeParse(set);
  if (!parsed.success) throw new ConfigError(parsed.error.issues);
  const e = parsed.data;
  const config: Config = {
    port: e.PORT,
    databaseUrl: e.DATABASE_URL,
    publicUrl: e.PUBLIC_URL,
    allowedHosts: e.ALLOWED_HOSTS,
    trustProxy: e.TRUST_PROXY,
    trustProxyHops: e.TRUST_PROXY_HOPS,
    cookieSecure: e.COOKIE_SECURE,
    secretsKey: e.SECRETS_KEY,
    oidc: {
      issuer: e.OIDC_ISSUER,
      clientId: e.OIDC_CLIENT_ID,
      clientSecret: e.OIDC_CLIENT_SECRET,
      loginGroup: e.OIDC_LOGIN_GROUP,
      adminGroup: e.OIDC_ADMIN_GROUP,
    },
    syncIntervalHours: e.SYNC_INTERVAL_HOURS,
    callLogRetentionDays: e.CALL_LOG_RETENTION_DAYS,
    upstreamMaxResponseBytes: e.UPSTREAM_MAX_RESPONSE_BYTES,
    mcpMaxBodyBytes: e.MCP_MAX_BODY_BYTES,
    embedder: e.EMBEDDER,
    modelDir: e.MODEL_DIR,
    searchMinSimilarity: e.SEARCH_MIN_SIMILARITY,
    logLevel: e.LOG_LEVEL,
  };
  if (e.SECRETS_KEY_OLD !== undefined) config.secretsKeyOld = e.SECRETS_KEY_OLD;
  if (e.EMBEDDER_URL !== undefined) config.embedderUrl = e.EMBEDDER_URL;
  if (e.EMBEDDER_MODEL !== undefined) config.embedderModel = e.EMBEDDER_MODEL;
  return config;
}
