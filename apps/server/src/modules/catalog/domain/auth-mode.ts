import { err, ok, type Result } from '../../../shared-kernel/index.ts';
import { CatalogError } from './errors.ts';

/** Client secret lives encrypted in `catalog.servers.secret`, not here. */
export interface OAuthClientConfig {
  authorizeUrl: string;
  tokenUrl: string;
  clientId: string;
  scopes: string[];
  revokeUrl?: string;
  /**
   * Added to the authorize request as is, e.g. `{ access_type: 'offline' }` for YouTrack Hub,
   * which issues a refresh token only then. May not override the flow's own parameters.
   */
  extraAuthorizeParams?: Record<string, string>;
}

/** `template` contains `{secret}`, e.g. `Bearer {secret}`. */
export type AuthMode =
  | { kind: 'none' }
  | { kind: 'shared_secret'; header: string; template: string }
  | { kind: 'oauth_user'; oauth: OAuthClientConfig; header: string; template: string }
  | { kind: 'pat_user'; header: string; template: string };

type CredentialSource = 'none' | 'server_secret' | 'personal';

const CREDENTIAL_SOURCE: Record<AuthMode['kind'], CredentialSource> = {
  none: 'none',
  shared_secret: 'server_secret',
  oauth_user: 'personal',
  pat_user: 'personal',
};

const PLACEHOLDER = '{secret}';
// RFC 9110 token characters, narrowed to what header names look like in practice.
const HEADER_NAME = /^[A-Za-z0-9-]+$/;
const BAD_TEMPLATE_CHARS = /[\r\n\0]/;

// Parameters the gateway sets itself on the authorize request; an extra one may not replace them.
const RESERVED_AUTHORIZE_PARAMS = new Set([
  'response_type',
  'client_id',
  'redirect_uri',
  'scope',
  'state',
  'code_challenge',
  'code_challenge_method',
]);
const PARAM_NAME = /^[A-Za-z0-9_.-]+$/;

const invalid = (message: string) => err(new CatalogError('invalid_auth', message));

function validHttpUrl(raw: string): boolean {
  try {
    const u = new URL(raw);
    return u.protocol === 'https:' || u.protocol === 'http:';
  } catch {
    return false;
  }
}

function checkHeader(header: string, template: string): CatalogError | null {
  if (!HEADER_NAME.test(header)) {
    return new CatalogError('invalid_auth', 'header name may contain letters, digits and dashes');
  }
  if (!template.includes(PLACEHOLDER) || BAD_TEMPLATE_CHARS.test(template)) {
    return new CatalogError(
      'invalid_auth',
      `header template must contain ${PLACEHOLDER} and be a single line`,
    );
  }
  return null;
}

function checkExtraParams(params: Record<string, unknown> | undefined): CatalogError | null {
  if (params === undefined) return null;
  for (const [key, value] of Object.entries(params)) {
    if (!PARAM_NAME.test(key) || RESERVED_AUTHORIZE_PARAMS.has(key)) {
      return new CatalogError('invalid_auth', `extra authorize parameter "${key}" is not allowed`);
    }
    if (typeof value !== 'string') {
      return new CatalogError('invalid_auth', 'extra authorize parameter values must be strings');
    }
  }
  return null;
}

function checkOAuth(o: OAuthClientConfig): CatalogError | null {
  if (!validHttpUrl(o.authorizeUrl) || !validHttpUrl(o.tokenUrl)) {
    return new CatalogError('invalid_auth', 'authorize and token URLs must be http(s) URLs');
  }
  if (o.revokeUrl !== undefined && !validHttpUrl(o.revokeUrl)) {
    return new CatalogError('invalid_auth', 'revoke URL must be an http(s) URL');
  }
  if (o.clientId.trim().length === 0) {
    return new CatalogError('invalid_auth', 'OAuth client id is required');
  }
  // With `openid` the provider adds an ID token, and its issuer cannot be checked: there is no
  // discovery, only the endpoints the admin entered. Upstream access needs no ID token anyway.
  if (o.scopes.some((s) => s.toLowerCase() === 'openid')) {
    return new CatalogError('invalid_auth', 'the openid scope is not supported for upstream OAuth');
  }
  return checkExtraParams(o.extraAuthorizeParams);
}

export const AuthMode = {
  /** Validates an auth mode; the single place that knows what a valid one looks like. */
  parse(mode: AuthMode): Result<AuthMode, CatalogError> {
    switch (mode.kind) {
      case 'none':
        return ok({ kind: 'none' });
      case 'shared_secret':
      case 'pat_user': {
        const bad = checkHeader(mode.header, mode.template);
        return bad ? err(bad) : ok(mode);
      }
      case 'oauth_user': {
        const bad = checkHeader(mode.header, mode.template) ?? checkOAuth(mode.oauth);
        return bad ? err(bad) : ok(mode);
      }
      default:
        return invalid('unknown auth mode');
    }
  },

  /** Whether the server itself holds a secret: the OAuth client secret or the shared key. */
  holdsSecret(mode: AuthMode): boolean {
    return mode.kind === 'shared_secret' || mode.kind === 'oauth_user';
  },

  /**
   * Where the upstream credential comes from: nothing, the server's own secret, or the person's
   * connection (OAuth token or PAT). The OAuth client secret is not an upstream credential.
   */
  credentialSource(mode: AuthMode): CredentialSource {
    return CREDENTIAL_SOURCE[mode.kind];
  },

  /** The only place that builds the upstream auth header. `none` sends nothing. */
  headerFor(mode: AuthMode, secret: string): Record<string, string> {
    if (mode.kind === 'none') return {};
    // split/join: a replacement string would interpret `$&` and friends in the secret.
    return { [mode.header]: mode.template.split(PLACEHOLDER).join(secret) };
  },
};
