import * as oidc from 'openid-client';
import type { Clock } from '../../../shared-kernel/index.ts';
import type { OAuthClientConfig } from '../../catalog/index.ts';
import { type OAuthClient, TokenEndpointError, type TokenSet } from '../application/ports.ts';

/**
 * Token endpoint calls give up after this long. A refresh runs under the connection's row lock,
 * so this also bounds how long other replicas wait for it.
 */
const TOKEN_ENDPOINT_TIMEOUT_SEC = 10;

/**
 * OAuth 2.0 Authorization Code + PKCE (S256) through `openid-client`, without discovery: the
 * admin enters the endpoints (Gitea, YouTrack Hub). The client authenticates at the token
 * endpoint with HTTP Basic, or not at all when it has no secret. No ID tokens: the catalog does
 * not allow the `openid` scope (the issuer below is not a real one to check them against).
 */
export class OpenidOAuthClient implements OAuthClient {
  constructor(private readonly clock: Clock) {}

  authorizeUrl(
    cfg: OAuthClientConfig,
    input: { redirectUri: string; state: string; codeChallenge: string },
  ): URL {
    return oidc.buildAuthorizationUrl(configuration(cfg, null), {
      // Extras first: the flow's own parameters always win (the catalog also forbids clashes).
      ...cfg.extraAuthorizeParams,
      response_type: 'code',
      redirect_uri: input.redirectUri,
      state: input.state,
      code_challenge: input.codeChallenge,
      code_challenge_method: 'S256',
      ...(cfg.scopes.length > 0 ? { scope: cfg.scopes.join(' ') } : {}),
    });
  }

  async exchangeCode(
    cfg: OAuthClientConfig,
    clientSecret: string | null,
    input: { code: string; codeVerifier: string; redirectUri: string },
  ): Promise<TokenSet> {
    // The state was matched against our own store already; only the code is replayed.
    const callback = new URL(input.redirectUri);
    callback.searchParams.set('code', input.code);
    const tokens = await tokenEndpoint(() =>
      oidc.authorizationCodeGrant(configuration(cfg, clientSecret), callback, {
        pkceCodeVerifier: input.codeVerifier,
      }),
    );
    return this.tokenSet(tokens);
  }

  async refresh(
    cfg: OAuthClientConfig,
    clientSecret: string | null,
    refreshToken: string,
  ): Promise<TokenSet> {
    return this.tokenSet(
      await tokenEndpoint(() =>
        oidc.refreshTokenGrant(configuration(cfg, clientSecret), refreshToken),
      ),
    );
  }

  async revoke(cfg: OAuthClientConfig, clientSecret: string | null, token: string): Promise<void> {
    await oidc.tokenRevocation(configuration(cfg, clientSecret), token);
  }

  private tokenSet(tokens: oidc.TokenEndpointResponse & oidc.TokenEndpointResponseHelpers) {
    const expiresIn = tokens.expiresIn();
    return {
      accessToken: tokens.access_token,
      refreshToken: tokens.refresh_token ?? null,
      expiresAt:
        expiresIn === undefined ? null : new Date(this.clock.now().getTime() + expiresIn * 1000),
    };
  }
}

/**
 * Turns the token endpoint's error answers into TokenEndpointError (status + OAuth `error`), so
 * the application can tell "the provider said no" from "no answer". Whether a "no" ends the
 * grant is decided in the domain (`isRefreshRejected`), not here. Anything else is rethrown.
 */
async function tokenEndpoint<T>(call: () => Promise<T>): Promise<T> {
  try {
    return await call();
  } catch (error) {
    if (error instanceof oidc.ResponseBodyError) {
      throw new TokenEndpointError(error.status, error.error);
    }
    if (error instanceof oidc.WWWAuthenticateChallengeError) {
      const code = error.cause[0]?.parameters.error;
      throw new TokenEndpointError(error.status, code ?? null);
    }
    // A non-JSON error answer: openid-client keeps the Response as the cause.
    if (error instanceof oidc.ClientError && error.cause instanceof Response) {
      throw new TokenEndpointError(error.cause.status, null);
    }
    throw error;
  }
}

/** `application/x-www-form-urlencoded` of one value (WHATWG: space → `+`; `-._*` stay). */
const formEncode = (value: string): string => new URLSearchParams({ v: value }).toString().slice(2);

/**
 * `Authorization: Basic base64(enc(id):enc(secret))` with each part form-encoded first, as
 * RFC 6749 §2.3.1 requires. `oidc.ClientSecretBasic` escapes more than that encoding does
 * (`-` → `%2D`, `.`, `_`, `~` too); a provider that does not decode Basic credentials would then
 * reject a UUID client id. With the WHATWG encoding, UUIDs and typical secrets go out unchanged,
 * and `+`, `%`, `:` in a secret are escaped so a decoding provider gets them back intact.
 */
function formBasic(clientId: string, clientSecret: string): oidc.ClientAuth {
  const pair = `${formEncode(clientId)}:${formEncode(clientSecret)}`;
  const value = `Basic ${Buffer.from(pair, 'utf8').toString('base64')}`;
  return (_as, _client, _body, headers) => {
    headers.set('authorization', value);
  };
}

function configuration(cfg: OAuthClientConfig, clientSecret: string | null): oidc.Configuration {
  const tokenUrl = new URL(cfg.tokenUrl);
  const config = new oidc.Configuration(
    {
      // No discovery and no ID tokens: the issuer only names the server for openid-client.
      issuer: tokenUrl.origin,
      authorization_endpoint: cfg.authorizeUrl,
      token_endpoint: cfg.tokenUrl,
      ...(cfg.revokeUrl === undefined ? {} : { revocation_endpoint: cfg.revokeUrl }),
    },
    cfg.clientId,
    undefined,
    clientSecret === null ? oidc.None() : formBasic(cfg.clientId, clientSecret),
  );
  config.timeout = TOKEN_ENDPOINT_TIMEOUT_SEC;
  // Plain http is for an upstream on a private network or in tests; https never reaches this.
  if (tokenUrl.protocol === 'http:') oidc.allowInsecureRequests(config);
  return config;
}
