import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vite-plus/test';
import { DEFAULT_MIN_SIMILARITY } from '../modules/search/index.ts';
import { ConfigError, loadConfig } from './config.ts';

const KEY = randomBytes(32).toString('base64');

const base = {
  DATABASE_URL: 'postgres://u:p@h/db',
  PUBLIC_URL: 'http://localhost:3000',
  SECRETS_KEY: KEY,
  OIDC_ISSUER: 'https://id.example.com',
  OIDC_CLIENT_ID: 'mcp-gateway',
  OIDC_CLIENT_SECRET: 'client-secret',
};

/** Parses a dotenv-like file: KEY=value lines, `#` comments and blank lines ignored. */
function parseEnvFile(text: string): Record<string, string> {
  const env: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (line === '' || line.startsWith('#')) continue;
    const eq = line.indexOf('=');
    if (eq < 0) throw new Error(`not a KEY=value line: ${line}`);
    env[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return env;
}

const files =
  (map: Record<string, string>) =>
  (path: string): string => {
    const text = map[path];
    if (text === undefined) throw new Error(`ENOENT ${path}`);
    return text;
  };

describe('loadConfig', () => {
  describe('secrets from files', () => {
    const { SECRETS_KEY: _unused, ...noKey } = base;
    it('reads SECRETS_KEY from SECRETS_KEY_FILE and trims the trailing newline', () => {
      const c = loadConfig({ ...noKey, SECRETS_KEY_FILE: '/k' }, files({ '/k': `${KEY}\n` }));
      expect(Buffer.from(c.secretsKey).toString('base64')).toBe(KEY);
    });

    it('reads the database password from PGPASSWORD_FILE', () => {
      const c = loadConfig({ ...base, PGPASSWORD_FILE: '/p' }, files({ '/p': 'pw\n' }));
      expect(c.databasePassword).toBe('pw');
    });

    it('refuses SECRETS_KEY together with SECRETS_KEY_FILE', () => {
      expect(() => loadConfig({ ...base, SECRETS_KEY_FILE: '/k' }, files({ '/k': KEY }))).toThrow(
        /SECRETS_KEY.*SECRETS_KEY_FILE/s,
      );
    });

    it('refuses PGPASSWORD together with PGPASSWORD_FILE', () => {
      expect(() =>
        loadConfig({ ...base, PGPASSWORD: 'x', PGPASSWORD_FILE: '/p' }, files({ '/p': 'pw' })),
      ).toThrow(/PGPASSWORD.*PGPASSWORD_FILE/s);
    });

    it('names the variable and the path when the file cannot be read', () => {
      let message = '';
      try {
        loadConfig({ ...noKey, SECRETS_KEY_FILE: '/run/missing' }, files({}));
      } catch (error) {
        message = error instanceof Error ? error.message : '';
      }
      expect(message).toContain('SECRETS_KEY_FILE');
      expect(message).toContain('/run/missing');
    });

    it('refuses an empty secret file', () => {
      expect(() =>
        loadConfig({ ...noKey, SECRETS_KEY_FILE: '/k' }, files({ '/k': ' \n\t' })),
      ).toThrow(/SECRETS_KEY_FILE/);
      expect(() => loadConfig({ ...base, PGPASSWORD_FILE: '/p' }, files({ '/p': '\n' }))).toThrow(
        /PGPASSWORD_FILE/,
      );
    });

    it('validates a key from a file like a key from the environment', () => {
      const short = randomBytes(16).toString('base64');
      let message = '';
      try {
        loadConfig({ ...noKey, SECRETS_KEY_FILE: '/k' }, files({ '/k': `${short}\n` }));
      } catch (error) {
        message = error instanceof Error ? error.message : '';
      }
      expect(message).toContain('SECRETS_KEY');
      expect(message).toContain('32 bytes');
      expect(message).not.toContain(short);
    });

    it('needs neither file variable', () => {
      const c = loadConfig(base, () => {
        throw new Error('must not read');
      });
      expect(c.databasePassword).toBeUndefined();
    });

    it('treats an empty _FILE variable as unset', () => {
      const c = loadConfig({ ...base, SECRETS_KEY_FILE: '', PGPASSWORD_FILE: '' }, () => {
        throw new Error('must not read');
      });
      expect(c.databasePassword).toBeUndefined();
    });
  });

  it('applies defaults', () => {
    expect(loadConfig(base)).toMatchObject({
      port: 3000,
      trustProxy: false,
      trustProxyHops: 1,
      cookieSecure: true,
      embedder: 'local',
      modelDir: '/app/models',
      logLevel: 'info',
      allowedHosts: [],
      // The search module's calibrated constant, not a copy of it.
      searchMinSimilarity: DEFAULT_MIN_SIMILARITY,
      syncIntervalHours: 6,
      callLogRetentionDays: 90,
      upstreamMaxResponseBytes: 20 * 1024 * 1024,
      mcpMaxBodyBytes: 10 * 1024 * 1024,
      oidc: {
        client: {
          issuer: 'https://id.example.com',
          clientId: 'mcp-gateway',
          clientSecret: 'client-secret',
        },
        loginGroup: 'mcp-users',
        adminGroup: 'mcp-admins',
        scopes: 'openid profile email groups',
        groupsClaim: 'groups',
      },
    });
  });

  it('parses overrides', () => {
    const c = loadConfig({
      ...base,
      PORT: '8080',
      ALLOWED_HOSTS: 'a.com, b.com',
      TRUST_PROXY: 'true',
      TRUST_PROXY_HOPS: '2',
      SEARCH_MIN_SIMILARITY: '0.5',
      OIDC_LOGIN_GROUP: 'staff',
      OIDC_ADMIN_GROUP: 'owners',
      OIDC_SCOPES: 'openid, profile  roles openid',
      OIDC_GROUPS_CLAIM: 'urn:zitadel:iam:org:project:roles',
      SYNC_INTERVAL_HOURS: '12',
      CALL_LOG_RETENTION_DAYS: '30',
      UPSTREAM_MAX_RESPONSE_BYTES: '1048576',
      MCP_MAX_BODY_BYTES: '2097152',
    });
    expect(c.port).toBe(8080);
    expect(c.allowedHosts).toEqual(['a.com', 'b.com']);
    expect(c.trustProxy).toBe(true);
    expect(c.trustProxyHops).toBe(2);
    expect(c.searchMinSimilarity).toBe(0.5);
    expect(c.oidc.loginGroup).toBe('staff');
    expect(c.oidc.adminGroup).toBe('owners');
    expect(c.oidc.scopes).toBe('openid profile roles');
    expect(c.oidc.groupsClaim).toBe('urn:zitadel:iam:org:project:roles');
    expect(c.syncIntervalHours).toBe(12);
    expect(c.callLogRetentionDays).toBe(30);
    expect(c.upstreamMaxResponseBytes).toBe(1_048_576);
    expect(c.mcpMaxBodyBytes).toBe(2_097_152);
  });

  it('decodes SECRETS_KEY into 32 bytes', () => {
    const c = loadConfig(base);
    expect(c.secretsKey).toBeInstanceOf(Uint8Array);
    expect(Buffer.from(c.secretsKey).toString('base64')).toBe(KEY);
  });

  it('SECRETS_KEY_OLD is optional, validated like SECRETS_KEY and must differ from it', () => {
    expect(loadConfig(base).secretsKeyOld).toBeUndefined();
    const old = randomBytes(32).toString('base64');
    const c = loadConfig({ ...base, SECRETS_KEY_OLD: old });
    expect(Buffer.from(c.secretsKeyOld ?? []).toString('base64')).toBe(old);
    expect(() => loadConfig({ ...base, SECRETS_KEY_OLD: 'short' })).toThrow(/SECRETS_KEY_OLD/);
    expect(() => loadConfig({ ...base, SECRETS_KEY_OLD: KEY })).toThrow(/SECRETS_KEY_OLD/);
  });

  it('rejects SECRETS_KEY that is not base64, naming the variable', () => {
    expect(() => loadConfig({ ...base, SECRETS_KEY: 'not base64 !!!' })).toThrow(/SECRETS_KEY/);
  });

  it('rejects SECRETS_KEY that is not 32 bytes, naming the variable', () => {
    const short = randomBytes(16).toString('base64');
    const long = randomBytes(33).toString('base64');
    expect(() => loadConfig({ ...base, SECRETS_KEY: short })).toThrow(/SECRETS_KEY/);
    expect(() => loadConfig({ ...base, SECRETS_KEY: long })).toThrow(/SECRETS_KEY/);
  });

  it('requires SECRETS_KEY and never prints its value', () => {
    expect(() => loadConfig({ ...base, SECRETS_KEY: '' })).toThrow(/SECRETS_KEY/);
    let message = '';
    try {
      loadConfig({ ...base, SECRETS_KEY: 'bad-secret-value-xyz' });
    } catch (e) {
      if (e instanceof Error) message = e.message;
    }
    expect(message).toContain('SECRETS_KEY');
    expect(message).not.toContain('bad-secret-value-xyz');
  });

  it('starts without OIDC when none of the three variables is set', () => {
    const { OIDC_ISSUER: _a, OIDC_CLIENT_ID: _b, OIDC_CLIENT_SECRET: _c, ...without } = base;
    expect(loadConfig(without).oidc.client).toBeNull();
    const empty = { ...base, OIDC_ISSUER: '', OIDC_CLIENT_ID: '', OIDC_CLIENT_SECRET: '' };
    const c = loadConfig(empty);
    expect(c.oidc.client).toBeNull();
    // The group policy keeps its defaults: provisionUser uses it without a provider.
    expect(c.oidc).toMatchObject({ loginGroup: 'mcp-users', adminGroup: 'mcp-admins' });
  });

  it('refuses a partial OIDC configuration and names what is missing', () => {
    let message = '';
    try {
      loadConfig({ ...base, OIDC_CLIENT_ID: '', OIDC_CLIENT_SECRET: '' });
    } catch (e) {
      if (e instanceof Error) message = e.message;
    }
    expect(message).toContain('OIDC_CLIENT_ID');
    expect(message).toContain('OIDC_CLIENT_SECRET');
    expect(message).not.toContain('OIDC_ISSUER:');
  });

  it('keeps a full OIDC configuration unchanged', () => {
    expect(loadConfig(base).oidc.client).toEqual({
      issuer: 'https://id.example.com',
      clientId: 'mcp-gateway',
      clientSecret: 'client-secret',
    });
  });

  it('requires OIDC_ISSUER', () => {
    expect(() => loadConfig({ ...base, OIDC_ISSUER: '' })).toThrow(/OIDC_ISSUER/);
    const { OIDC_ISSUER: _omitted, ...without } = base;
    expect(() => loadConfig(without)).toThrow(/OIDC_ISSUER/);
  });

  it('requires OIDC_CLIENT_ID and OIDC_CLIENT_SECRET', () => {
    expect(() => loadConfig({ ...base, OIDC_CLIENT_ID: '' })).toThrow(/OIDC_CLIENT_ID/);
    expect(() => loadConfig({ ...base, OIDC_CLIENT_SECRET: '' })).toThrow(/OIDC_CLIENT_SECRET/);
  });

  it('bounds the numeric settings', () => {
    expect(() => loadConfig({ ...base, TRUST_PROXY_HOPS: '0' })).toThrow(ConfigError);
    expect(() => loadConfig({ ...base, TRUST_PROXY_HOPS: '6' })).toThrow(ConfigError);
    expect(() => loadConfig({ ...base, TRUST_PROXY_HOPS: '1.5' })).toThrow(ConfigError);
    expect(() => loadConfig({ ...base, SEARCH_MIN_SIMILARITY: '1.5' })).toThrow(/SEARCH_MIN/);
    expect(() => loadConfig({ ...base, SEARCH_MIN_SIMILARITY: '-0.1' })).toThrow(/SEARCH_MIN/);
    expect(() => loadConfig({ ...base, SYNC_INTERVAL_HOURS: '0' })).toThrow(/SYNC_INTERVAL_HOURS/);
    // A year at most: beyond that the value is a typo, and timers stay far from overflow.
    expect(loadConfig({ ...base, SYNC_INTERVAL_HOURS: '8760' }).syncIntervalHours).toBe(8760);
    expect(() => loadConfig({ ...base, SYNC_INTERVAL_HOURS: '8761' })).toThrow(
      /SYNC_INTERVAL_HOURS/,
    );
    expect(() => loadConfig({ ...base, CALL_LOG_RETENTION_DAYS: '0' })).toThrow(
      /CALL_LOG_RETENTION_DAYS/,
    );
    expect(() => loadConfig({ ...base, UPSTREAM_MAX_RESPONSE_BYTES: '0' })).toThrow(
      /UPSTREAM_MAX_RESPONSE_BYTES/,
    );
    expect(() => loadConfig({ ...base, MCP_MAX_BODY_BYTES: '0' })).toThrow(/MCP_MAX_BODY_BYTES/);
  });

  it('rejects a missing database url', () => {
    expect(() => loadConfig({ ...base, DATABASE_URL: '' })).toThrow(/DATABASE_URL/);
  });

  it('treats empty optional values as unset', () => {
    const c = loadConfig({
      ...base,
      EMBEDDER_URL: '',
      EMBEDDER_MODEL: '',
      ALLOWED_HOSTS: '',
      TRUST_PROXY: '',
      PORT: '',
      OIDC_LOGIN_GROUP: '',
      SYNC_INTERVAL_HOURS: '',
    });
    expect(c.embedderUrl).toBeUndefined();
    expect(c.embedderModel).toBeUndefined();
    expect(c.trustProxy).toBe(false);
    expect(c.port).toBe(3000);
    expect(c.oidc.loginGroup).toBe('mcp-users');
    expect(c.syncIntervalHours).toBe(6);
  });

  it('refuses OIDC_SCOPES without openid', () => {
    expect(() => loadConfig({ ...base, OIDC_SCOPES: 'profile groups' })).toThrow(
      /OIDC_SCOPES.*openid/,
    );
  });

  it('reports every bad variable by name, one per line', () => {
    let message = '';
    try {
      loadConfig({ ...base, PORT: 'abc', PUBLIC_URL: 'not a url' });
    } catch (e) {
      expect(e).toBeInstanceOf(ConfigError);
      if (e instanceof Error) message = e.message;
    }
    expect(message).toContain('PORT');
    const lines = message.split('\n');
    expect(lines.some((l) => l.includes('PORT') && !l.includes('PUBLIC_URL'))).toBe(true);
    expect(lines.some((l) => l.includes('PUBLIC_URL') && !l.includes('PORT:'))).toBe(true);
  });

  it('accepts the shipped .env.example once the required values are filled in', () => {
    const file = readFileSync(new URL('../../../../.env.example', import.meta.url), 'utf8');
    const env = parseEnvFile(file);
    const c = loadConfig({
      ...env,
      DATABASE_URL: 'postgres://mcp@db:5432/mcp',
      SECRETS_KEY: KEY,
      OIDC_ISSUER: 'https://id.example.com',
      OIDC_CLIENT_ID: 'id',
      OIDC_CLIENT_SECRET: 'secret',
    });
    expect(c.port).toBe(3000);
    expect(c.oidc.loginGroup).toBe('mcp-users');
  });
});
