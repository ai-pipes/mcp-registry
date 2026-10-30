import { createHash, randomUUID } from 'node:crypto';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';

interface Grant {
  challenge: string;
  redirectUri: string;
}

/**
 * A minimal OAuth 2.0 provider (Gitea / YouTrack Hub shape) on a local port: the token endpoint
 * (authorization_code with PKCE S256, refresh_token) with HTTP Basic client authentication, and
 * RFC 7009 revocation. The "browser" step (`authorize`) is the test.
 */
export class FakeOAuthServer {
  readonly clientId = 'gateway-client';
  /** `+`, `%` and `:` survive only if the client form-encodes them before Basic (RFC 6749 §2.3.1). */
  readonly clientSecret = 'gate+way%2F:secret';
  url = '';
  /** Every refresh_token grant received, in order. */
  readonly refreshes: string[] = [];
  readonly revoked: string[] = [];
  /** Answers refresh_token grants with 400 invalid_grant. */
  refreshFails = false;
  /** Answers refresh_token grants with a bare HTTP 503 (no OAuth error body). */
  refreshUnavailable = false;
  /** Holds refresh answers this long, so concurrent callers overlap. */
  refreshDelayMs = 0;
  private server: Server | undefined;
  private readonly grants = new Map<string, Grant>();
  private readonly refreshTokens = new Set<string>();

  async start(): Promise<void> {
    this.server = createServer((req, res) => {
      const path = new URL(req.url ?? '/', 'http://x').pathname;
      if (req.method !== 'POST') {
        res.writeHead(404).end();
        return;
      }
      const chunks: Buffer[] = [];
      req.on('data', (c: Buffer) => chunks.push(c));
      req.on('end', () => {
        const form = new URLSearchParams(Buffer.concat(chunks).toString());
        if (!this.clientAuthenticated(req)) return json(res, 401, { error: 'invalid_client' });
        if (path === '/token') return void this.token(form, res);
        if (path === '/revoke') {
          this.revoked.push(form.get('token') ?? '');
          return void res.writeHead(200).end();
        }
        res.writeHead(404).end();
      });
    });
    await new Promise<void>((resolve) => this.server?.listen(0, '127.0.0.1', resolve));
    const address = this.server?.address();
    if (address === null || address === undefined || typeof address === 'string') {
      throw new Error('fake OAuth server did not bind a port');
    }
    this.url = `http://127.0.0.1:${address.port}`;
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolve) => {
      this.server?.close(() => resolve());
      this.server?.closeAllConnections();
    });
  }

  /** What the person's browser does at the provider: log in, consent, get a code. */
  authorize(authorizeUrl: URL): string {
    const p = authorizeUrl.searchParams;
    if (
      p.get('client_id') !== this.clientId ||
      p.get('response_type') !== 'code' ||
      p.get('code_challenge_method') !== 'S256' ||
      !p.get('code_challenge') ||
      !p.get('state')
    ) {
      throw new Error('bad authorization request');
    }
    const code = randomUUID();
    this.grants.set(code, {
      challenge: p.get('code_challenge') ?? '',
      redirectUri: p.get('redirect_uri') ?? '',
    });
    return code;
  }

  /** Decodes Basic credentials as RFC 6749 §2.3.1 says: base64, split at `:`, form-decode both. */
  private clientAuthenticated(req: IncomingMessage): boolean {
    const header = req.headers.authorization ?? '';
    if (!header.startsWith('Basic ')) return false;
    const pair = Buffer.from(header.slice(6), 'base64').toString('utf8');
    const colon = pair.indexOf(':');
    if (colon < 0) return false;
    return (
      formDecode(pair.slice(0, colon)) === this.clientId &&
      formDecode(pair.slice(colon + 1)) === this.clientSecret
    );
  }

  private async token(form: URLSearchParams, res: ServerResponse): Promise<void> {
    if (form.get('grant_type') === 'refresh_token') {
      const token = form.get('refresh_token') ?? '';
      this.refreshes.push(token);
      if (this.refreshDelayMs > 0) await new Promise((r) => setTimeout(r, this.refreshDelayMs));
      if (this.refreshUnavailable) return void res.writeHead(503).end('maintenance');
      if (this.refreshFails || !this.refreshTokens.has(token)) {
        return json(res, 400, { error: 'invalid_grant' });
      }
      // Like Hub and Gitea by default: the refresh token is not rotated and not sent back.
      return json(res, 200, {
        access_token: `access-${randomUUID()}`,
        token_type: 'bearer',
        expires_in: 3600,
      });
    }
    const code = form.get('code') ?? '';
    const grant = this.grants.get(code);
    this.grants.delete(code);
    const verifier = form.get('code_verifier') ?? '';
    const challenge = createHash('sha256').update(verifier).digest('base64url');
    if (
      form.get('grant_type') !== 'authorization_code' ||
      !grant ||
      challenge !== grant.challenge ||
      form.get('redirect_uri') !== grant.redirectUri
    ) {
      return json(res, 400, { error: 'invalid_grant' });
    }
    const refreshToken = `refresh-${randomUUID()}`;
    this.refreshTokens.add(refreshToken);
    json(res, 200, {
      access_token: `access-${randomUUID()}`,
      refresh_token: refreshToken,
      token_type: 'bearer',
      expires_in: 3600,
    });
  }
}

function formDecode(value: string): string | null {
  return new URLSearchParams(`v=${value}`).get('v');
}

function json(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(JSON.stringify(body));
}
