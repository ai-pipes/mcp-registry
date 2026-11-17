# mcp-registry

A self-hosted MCP gateway: one MCP endpoint for your AI agents (Claude Code and other MCP clients) in front of all the MCP servers your team uses.

Instead of loading hundreds of upstream tools into the agent's context, the gateway exposes four meta-tools. The agent searches for the tool it needs, reads its schema, and calls it through the gateway. Each call is made on behalf of the person: with their own OAuth token or personal access token for that service, or with a shared server secret that never leaves the gateway.

**Who it is for.** Individuals and small teams who run their own services (Gitea, YouTrack, internal tools) inside their own network and want one place to manage MCP access. It is a single container plus Postgres. It is **not** an enterprise product: no multi-tenancy, no HA clustering, no fine-grained RBAC beyond "member / admin / server visible to groups".

## How it works

```mermaid
flowchart LR
  A[Claude Code<br/>Bearer sk_...] -->|/mcp| G[mcp-registry]
  B[Browser] -->|OIDC login| G
  G -->|person's OAuth token / PAT<br/>or shared secret| U1[Gitea MCP]
  G --> U2[YouTrack MCP]
  G --> U3[Other MCP servers]
  G --- DB[(Postgres + pgvector)]
```

The agent sees exactly four tools, whatever the catalog contains:

| Tool            | What it does                                                                                  |
| --------------- | --------------------------------------------------------------------------------------------- |
| `list_servers`  | Servers the person may use, and whether they still need to connect an account (with a link).  |
| `search_tools`  | Hybrid search (full-text + vector) over tool descriptions; returns `<server>.<tool>` refs.    |
| `describe_tool` | Full description and `inputSchema` of one tool.                                               |
| `call_tool`     | Calls the upstream tool with the person's credentials; the upstream result is returned as is. |

Search returns an empty list when nothing is relevant enough, rather than padding results with near misses.

## Features

- UI login through any OIDC provider that can put the person's groups or roles into a claim of the ID token or UserInfo; no passwords stored.
- Personal API tokens (`sk_...`) for `/mcp`; the UI shows a ready-to-paste `claude mcp add` command.
- Server catalog: add upstream MCP servers, restrict them to groups, hide individual tools, set per-call timeouts and per-person daily limits.
- Per-user credentials: OAuth 2.0 (Authorization Code + PKCE, automatic refresh) or personal access tokens; or one shared secret per server.
- Secrets encrypted at rest with AES-256-GCM; key rotation without downtime.
- Catalog synchronisation with upstream every 6 hours (configurable) and on demand.
- Local embeddings (`multilingual-e5-small`, baked into the image; no external service, works offline). An OpenAI-compatible embeddings API can be used instead.
- Call log (who, which tool, outcome, duration; never arguments or results), audit log of admin actions.
- Strict CSP web UI (`default-src 'self'`, no inline scripts or styles).

## Quick start (Docker Compose)

Requirements: Docker with Compose, and (for anything beyond localhost) a TLS-terminating reverse proxy in front of the gateway.

```sh
git clone https://github.com/ai-pipes/mcp-registry.git && cd mcp-registry
cp .env.example .env
```

Edit `.env`:

```sh
PUBLIC_URL=https://mcp.example.com
TRUST_PROXY=true                       # behind a reverse proxy
POSTGRES_PASSWORD=$(openssl rand -base64 32)
SECRETS_KEY=$(openssl rand -base64 32) # back this up: without it stored secrets are lost
# Optional: sign in through your OIDC provider. Set all three or none (see "First admin").
OIDC_ISSUER=https://id.example.com
OIDC_CLIENT_ID=...
OIDC_CLIENT_SECRET=...
```

(Generate the values with `openssl rand -base64 32` and paste them in; `.env` does not run shell commands.)

Start it:

```sh
docker compose up -d --build
curl http://localhost:3000/healthz   # ok
```

Compose runs the app and `pgvector/pgvector:pg17`; Postgres is not published. Migrations run on startup. A prebuilt image is published to `ghcr.io/ai-pipes/mcp-registry` for tagged releases.

### First admin

**With OIDC** (`OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` set): put yourself in the admin group of your provider (`OIDC_ADMIN_GROUP`, default `mcp-admins`) and sign in at `PUBLIC_URL`. Role and groups are taken from the provider on every login.

**Without OIDC** (none of the three set): on the first start the server creates the admin `local-admin` and logs a one-time sign-in link. Open it within 15 minutes:

```sh
docker compose logs app | grep "one-time link"   # the link is in the "url" field
```

The link looks like `https://mcp.example.com/login#link=ll_...`; it works once. The page asks for one confirmation click before it signs you in. The link is logged at `warn` level, so with `LOG_LEVEL` set to `error` or stricter use `login-link local-admin` instead. Until `local-admin` has signed in, every start logs a new link (each invalidates the previous one). To sign in again, or to let another person in, print a link with the CLI; `<sub>` is the person's subject ID (for `create-user`, any name you choose):

```sh
docker compose exec app node dist/cli.js create-user --sub jane --name "Jane"   # new person; add --admin for an admin
docker compose exec app node dist/cli.js login-link jane
```

`login-link` also works with OIDC configured (emergency access). Links are valid for 15 minutes and a person has at most one live link. Anyone who can read the container log right after the first start can use the link, so see [SECURITY.md](SECURITY.md#running-without-oidc). Without OIDC there is no group management: admins see every active server, members see servers open to everyone or to the login group. To move to OIDC later, see [SECURITY.md](SECURITY.md#switching-to-oidc-later).

With OIDC you can also create a person with the CLI, for example when the provider is down. `<sub>` is the person's subject ID at the provider:

```sh
docker compose exec app node dist/cli.js create-user --sub <sub> --name "Jane" --admin
```

### Connect an agent

In the UI, open **Tokens**, create a token and copy the command it shows. Or from the CLI:

```sh
docker compose exec app node dist/cli.js create-token <sub> --name laptop
```

Then:

```sh
claude mcp add --transport http mcp-registry https://mcp.example.com/mcp \
  --header "Authorization: Bearer sk_..."
```

## Configuration

All settings are environment variables (see `.env.example`). An empty value (`KEY=`) counts as unset.

| Variable                      | Default                            | Meaning                                                                                                                                                          |
| ----------------------------- | ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PUBLIC_URL`                  | required                           | External URL of the gateway (scheme + host + port). Used for OIDC/OAuth redirect URIs, the `claude mcp add` command and `Origin` checks.                         |
| `DATABASE_URL`                | required (Compose sets it)         | Postgres connection string. Compose defaults to `postgres://mcp@db:5432/mcp` with `POSTGRES_PASSWORD`.                                                           |
| `POSTGRES_PASSWORD`           | required by Compose                | Password of the bundled Postgres. Set it before the first start; it is stored in the volume afterwards.                                                          |
| `PORT`                        | `3000`                             | Listening port (with Compose: the host port; the container always listens on 3000).                                                                              |
| `BIND_ADDRESS`                | `127.0.0.1`                        | Host address the port is published on. `quickstart.compose.yml` only; set `0.0.0.0` (with a real `PUBLIC_URL`, behind HTTPS) to expose it.                       |
| `ALLOWED_HOSTS`               | empty                              | Extra accepted `Host` values for `/mcp`, comma-separated. The `PUBLIC_URL` host, `localhost` and `127.0.0.1` are always accepted.                                |
| `TRUST_PROXY`                 | `false`                            | Take the client IP from `X-Forwarded-For`. Enable only behind a reverse proxy.                                                                                   |
| `TRUST_PROXY_HOPS`            | `1`                                | Number of trusted proxies in front of the gateway (1-5).                                                                                                         |
| `COOKIE_SECURE`               | `true`                             | `Secure` flag on the session cookie. Works with HTTPS and with `http://localhost` in Chrome and Firefox; Safari and plain http on a LAN address need `false`.    |
| `SECRETS_KEY`                 | required unless `SECRETS_KEY_FILE` | AES-256-GCM key, base64 of exactly 32 bytes (`openssl rand -base64 32`). Encrypts server secrets and people's tokens.                                            |
| `SECRETS_KEY_FILE`            | unset                              | Path of a file holding the key instead of `SECRETS_KEY` (not both). `quickstart.compose.yml` sets it; for your own compose file or orchestrator.                 |
| `PGPASSWORD_FILE`             | unset                              | Path of a file holding the database password (not together with `PGPASSWORD`). `quickstart.compose.yml` sets it; the bundled `docker-compose.yml` cannot use it. |
| `SECRETS_KEY_OLD`             | unset                              | Previous key, only during key rotation. Must differ from `SECRETS_KEY`.                                                                                          |
| `OIDC_ISSUER`                 | unset                              | Issuer URL of the OIDC provider. Set all three `OIDC_*` or none; without them people sign in with one-time links. Discovery happens on the first login.          |
| `OIDC_CLIENT_ID`              | unset                              | Client ID registered for the gateway (with `OIDC_ISSUER` and `OIDC_CLIENT_SECRET`).                                                                              |
| `OIDC_CLIENT_SECRET`          | unset                              | Client secret registered for the gateway (with `OIDC_ISSUER` and `OIDC_CLIENT_ID`).                                                                              |
| `OIDC_LOGIN_GROUP`            | `mcp-users`                        | Members of this group may sign in.                                                                                                                               |
| `OIDC_ADMIN_GROUP`            | `mcp-admins`                       | Members of this group may sign in and are admins.                                                                                                                |
| `OIDC_SCOPES`                 | `openid profile email groups`      | Scopes of the login request, space- or comma-separated; must include `openid`.                                                                                   |
| `OIDC_GROUPS_CLAIM`           | `groups`                           | Claim with the person's groups: a list of names, or an object whose keys are the names.                                                                          |
| `SYNC_INTERVAL_HOURS`         | `6`                                | Catalog synchronisation period, hours (1-8760).                                                                                                                  |
| `CALL_LOG_RETENTION_DAYS`     | `90`                               | How long call log entries are kept.                                                                                                                              |
| `UPSTREAM_MAX_RESPONSE_BYTES` | `20971520` (20 MiB)                | Larger upstream responses are refused (memory protection).                                                                                                       |
| `MCP_MAX_BODY_BYTES`          | `10485760` (10 MiB)                | Request body limit of `/mcp` (tool arguments may carry base64 files). The rest of the HTTP API is limited to 1 MiB.                                              |
| `EMBEDDER`                    | `local`                            | `local` (bundled model) or `openai-compatible` (`POST <EMBEDDER_URL>/embeddings`).                                                                               |
| `EMBEDDER_URL`                | unset                              | Base URL of the embeddings API, only for `openai-compatible`.                                                                                                    |
| `EMBEDDER_MODEL`              | unset                              | Model name for `openai-compatible`. Must produce 384-dimensional vectors.                                                                                        |
| `MODEL_DIR`                   | `/app/models`                      | Directory of the local model. The image already contains it.                                                                                                     |
| `SEARCH_MIN_SIMILARITY`       | `0.83`                             | Cosine-similarity floor (0-1) for vector search candidates.                                                                                                      |
| `LOG_LEVEL`                   | `info`                             | `fatal`, `error`, `warn`, `info`, `debug`, `trace` or `silent`.                                                                                                  |

## OIDC setup

Optional: skip this section to run without a provider (see "First admin").

Any OIDC provider works if it can put the person's groups (or roles) into one claim of the ID token or the UserInfo response: a flat list of names, or an object whose keys are the names. The claim name is `OIDC_GROUPS_CLAIM` (default `groups`), the requested scopes are `OIDC_SCOPES` (default `openid profile email groups`).

1. Create two groups: the login group (`mcp-users`) and the admin group (`mcp-admins`). Membership in either lets a person in; the admin group also makes them an admin.
2. Create a confidential OIDC client:
   - redirect URI: `https://<host>/api/auth/callback`;
   - PKCE: allowed (the gateway always sends S256);
   - make sure the groups claim is emitted (see the table below).
3. Put the issuer, client ID and client secret into `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, and set `OIDC_SCOPES` / `OIDC_GROUPS_CLAIM` if your provider needs it.

| Provider                               | Settings                                                                                                                                                                 |
| -------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Pocket ID, Authentik, Authelia, Kanidm | defaults                                                                                                                                                                 |
| Keycloak                               | defaults; add a client scope `groups` with a Group Membership mapper (token claim name `groups`, "Full group path" off) and assign it to the client                      |
| Zitadel                                | `OIDC_SCOPES=openid profile email urn:zitadel:iam:org:projects:roles`, `OIDC_GROUPS_CLAIM=urn:zitadel:iam:org:project:roles`; use project roles named like the groups    |
| Microsoft Entra ID                     | `OIDC_SCOPES=openid profile email`, `OIDC_GROUPS_CLAIM=roles`; define app roles named like the groups and assign them (the `groups` claim carries object IDs, not names) |
| Auth0                                  | `OIDC_SCOPES=openid profile email`, `OIDC_GROUPS_CLAIM=https://<your-namespace>/groups`; add the claim to the ID token with a post-login Action                          |

These are starting points, not tested recipes; check the claims your provider actually issues (for example on its token preview page) if login answers `/?loginError=not_allowed`.

Check the issuer with `curl -s https://id.example.com/.well-known/openid-configuration`. If login redirects to `/?loginError=idp_unavailable`, the gateway cannot reach `OIDC_ISSUER`; `/mcp` and existing tokens keep working.

Disabling a person in the UI revokes all their sessions and tokens.

## Adding upstream servers

Upstreams must speak **Streamable HTTP**. Each server has a `slug` (kebab-case, the prefix of its tool refs, cannot be changed later), a URL, an auth mode, access (everyone or a list of groups), a call timeout and an optional daily limit per person.

| Auth mode       | Credentials                                                                                                       |
| --------------- | ----------------------------------------------------------------------------------------------------------------- |
| `none`          | No header is sent.                                                                                                |
| `shared_secret` | One secret for the server, set by an admin, sent by the gateway for everyone.                                     |
| `pat_user`      | Each person stores their own personal access token in **Connections**.                                            |
| `oauth_user`    | Each person connects their account via OAuth 2.0 (Authorization Code + PKCE); tokens are refreshed automatically. |

For all modes except `none` you set a header name and a template containing `{secret}`, e.g. `Authorization` / `Bearer {secret}`. Secrets in the form are write-only.

**Order matters:** add the server, then connect your own account in **Connections**, then press **Sync**. For `oauth_user` and `pat_user` servers the first sync right after creation reports that an admin connection is needed; that is expected. Scheduled syncs use the connection of the last admin who synced the server successfully; if that person loses the admin role, another admin with a connection must press Sync once.

### Gitea (`oauth_user`)

1. Run `gitea-mcp` in HTTP mode, with no global token (each request carries the person's token), reachable from the gateway and not published:

   ```yaml
   gitea-mcp:
     image: docker.gitea.com/gitea-mcp-server
     command: ['/app/gitea-mcp', '-t', 'http', '--port', '8080', '-H', 'https://gitea.example.com']
   ```

2. In Gitea: Site Administration -> Applications -> OAuth2 Applications -> create an app, **Confidential Client** on, redirect URI `https://<host>/api/connections/callback`. Save the client ID and secret.
3. Add the server in the gateway:

   | Field             | Value                                                |
   | ----------------- | ---------------------------------------------------- |
   | URL               | `http://gitea-mcp:8080/mcp`                          |
   | Auth mode         | `oauth_user`                                         |
   | Authorize URL     | `https://gitea.example.com/login/oauth/authorize`    |
   | Token URL         | `https://gitea.example.com/login/oauth/access_token` |
   | Revoke URL        | empty (Gitea has no RFC 7009 endpoint)               |
   | Client ID, secret | from step 2                                          |
   | Scopes            | empty (Gitea then grants the user's full access)     |
   | Header / template | `Authorization` / `Bearer {secret}`                  |

A request with an invalid token to `gitea-mcp` returns HTTP 401; a connection error instead means the containers are not on the same network.

### YouTrack (`oauth_user`)

YouTrack has a built-in MCP endpoint at `/mcp`. OAuth goes through Hub (built into YouTrack at `<youtrack>/hub`, or standalone).

1. Hub -> Services -> New service: redirect URI `https://<host>/api/connections/callback`, set a secret. The service ID is the client ID.
2. Note the ID of the **YouTrack** service in Hub: it is the scope.
3. Add the server:

   | Field                      | Value                                                                                                                 |
   | -------------------------- | --------------------------------------------------------------------------------------------------------------------- |
   | URL                        | `https://youtrack.example.com/mcp`                                                                                    |
   | Auth mode                  | `oauth_user`                                                                                                          |
   | Authorize URL              | `<hub>/api/rest/oauth2/auth`                                                                                          |
   | Token URL                  | `<hub>/api/rest/oauth2/token`                                                                                         |
   | Client ID, secret          | from step 1                                                                                                           |
   | Scopes                     | the YouTrack service ID from step 2                                                                                   |
   | Extra authorize parameters | `access_type=offline` (**required**: without it Hub issues no refresh token and the connection expires after an hour) |
   | Header / template          | `Authorization` / `Bearer {secret}`                                                                                   |

### A server with a shared key (`shared_secret`)

For an MCP server protected by one API key: auth mode `shared_secret`, header e.g. `Authorization` with template `Bearer {secret}` (or `X-API-Key` / `{secret}`), and the key as the server secret. Every permitted person calls it with the same key; the call log still records who called what.

Servers can also be created from a JSON file (`ServerInput` in `packages/contracts/src/servers.ts`); the secret is read from stdin or an environment variable, never from the command line:

```sh
docker compose exec -T app node dist/cli.js create-server --json /path/in/container/server.json \
  --as <admin-sub> --secret-env UPSTREAM_KEY
```

## Operations

### CLI

Inside the container: `node dist/cli.js <command>` (same environment as the server). Exit codes: 0 success, 1 operation failed, 2 bad invocation.

| Command                                                             | Purpose                                                                                                                      |
| ------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `create-user --sub <sub> --name <name> [--admin]`                   | Create or update a person as a first login would. Idempotent by `sub`; without `--admin` an existing admin becomes a member. |
| `create-token <sub> --name <name>`                                  | Issue an `sk_` token and print the `claude mcp add` command.                                                                 |
| `login-link <sub>`                                                  | Print a one-time sign-in link (15 minutes) for an existing, enabled person. For servers without OIDC, or emergency access.   |
| `create-server --json <file> --as <admin-sub> [--secret-env <VAR>]` | Create a server and sync it. The secret comes from `<VAR>` or stdin.                                                         |
| `sync <slug> [--as <admin-sub>]`                                    | Sync a server's tools (without `--as`: as the scheduler).                                                                    |
| `reindex`                                                           | Rebuild the search index (if `search_tools` misses a tool that is in the catalog).                                           |
| `rotate-secrets-key`                                                | Re-encrypt every stored secret from `SECRETS_KEY_OLD` to `SECRETS_KEY`. Safe to repeat.                                      |
| `reset-key-check --yes`                                             | Remove the stored key check so the next start records the configured `SECRETS_KEY` (see "Recovering from a key mismatch").   |
| `--version`                                                         | Print the gateway version (needs no configuration and no database).                                                          |

### Rotating `SECRETS_KEY`

1. Generate a new key and keep the old one until rotation is done.
2. Set `SECRETS_KEY_OLD=<current key>`, `SECRETS_KEY=<new key>`, restart. Both keys open secrets meanwhile, so calls keep working; a warning is logged on each start.
3. Run `node dist/cli.js rotate-secrets-key`. It reports how many server secrets and connections were re-sealed and a `key check:` line (`re-sealed`, `already current`, `none stored` or `cannot be read with either key`). Exit code 1 lists records that neither key opens (wrong old key?).
4. Run it again: it should report `0 re-sealed` and `key check: already current`. Remove `SECRETS_KEY_OLD` and restart.

**Quick start variant.** The key lives in the `secrets` volume (`SECRETS_KEY_FILE`), so the new key is written there and the old one is passed as `SECRETS_KEY_OLD`. Run from the directory with the saved `quickstart.compose.yml`; add `--env-file my.env` to every `docker compose -f ...` command if you use one. Every command below was run against a quick-start stack:

```sh
OLD=$(docker compose -p mcp-registry exec -T app cat /secrets/secrets_key)   # keep this value until the end
NEW=$(openssl rand -base64 32)
docker compose -p mcp-registry down                                           # not "down -v"
printf '%s\n' "$NEW" | docker run --rm -i -v mcp-registry_secrets:/secrets \
  --entrypoint sh pgvector/pgvector:pg17 -c 'cat > /secrets/secrets_key'
SECRETS_KEY_OLD="$OLD" docker compose -f quickstart.compose.yml up -d
docker compose -p mcp-registry exec -T app node dist/cli.js rotate-secrets-key   # run twice
docker compose -f quickstart.compose.yml up -d                                # without SECRETS_KEY_OLD: the app is recreated
```

A successful run prints `server secrets: N re-sealed ...`, `connections: ...` and `key check: re-sealed` (then `already current` on the repeat). If the new key was not written correctly the app refuses to start; see below.

Back up `SECRETS_KEY` together with the database: a database restored without its key has no usable secrets or connections.

### Key check and secrets from files

At every start the server compares `SECRETS_KEY` with a check value kept in the database. The check proves that the key is the one that was recorded on the first start with the check; it does not test the stored secrets themselves. On the first start, and the first start after upgrading from 0.1.0, the value is created from whatever key is configured at that moment. If the key opens it, the server continues. If only `SECRETS_KEY_OLD` opens it, the server continues and logs a warning to run `rotate-secrets-key`. If neither opens it, the server logs one fatal record and exits with code 1. The server never generates or replaces the key itself.

`SECRETS_KEY_FILE` and `PGPASSWORD_FILE` read the key and the database password from files (for example Docker secrets); surrounding whitespace is trimmed. Set either the variable or its `_FILE` twin, not both: that is a startup error, as is an unreadable or empty file. Errors name the variable (`SECRETS_KEY_FILE` for a bad key read from a file) and the path, never the content.

CLI commands warn on stderr when the stored check does not match the configured key; `create-server`, which would seal a new secret with the wrong key, is refused (exit 1). `rotate-secrets-key` and `reset-key-check` run regardless.

### Recovering from a key mismatch

The app logs `SECRETS_KEY does not match the data in this database; ...` and restarts in a loop. First find out which case you are in.

**1. The server pinned a wrong key on the first start after an upgrade.** The check value is created from whatever key is configured on the first start with the check; it does not test existing secrets. If that key was wrong, the right key now looks like a mismatch. Put the right key back (`SECRETS_KEY` in `.env`, or the `secrets_key` file), remove the check, start:

```sh
docker compose run --rm app node dist/cli.js reset-key-check --yes   # docker-compose.yml + .env
docker compose up -d
```

For the quick start, the key is the `secrets_key` file in the `secrets` volume. Stop the stack, write the right key into the file (the same command as in the rotation section, with `$KEY` holding the right key), remove the check and start:

```sh
docker compose -p mcp-registry down                                           # not "down -v"
printf '%s\n' "$KEY" | docker run --rm -i -v mcp-registry_secrets:/secrets \
  --entrypoint sh pgvector/pgvector:pg17 -c 'cat > /secrets/secrets_key'
docker compose -p mcp-registry -f quickstart.compose.yml run --rm app node dist/cli.js reset-key-check --yes
docker compose -p mcp-registry -f quickstart.compose.yml up -d
```

Run the `-f quickstart.compose.yml` commands (here and in case 3) with the same `--env-file` and environment (including `MCP_REGISTRY_IMAGE`, if you set it) as when you started the stack; otherwise Compose recreates the stack with default settings.

When the app is in a restart loop `docker compose exec` fails ("container is restarting"); `run --rm` starts a separate one-off container with the same configuration and does not need the app to be up (the database starts if it is not running). The command prints `key check removed: the next start records the configured SECRETS_KEY`; the running app picks the new check up on its next restart attempt.

**2. The key is lost for good.** Server secrets and personal connections cannot be recovered. Put a new key in place (`openssl rand -base64 32`) and run the same `reset-key-check --yes`. People, tokens, the catalog and the call log remain; every server secret (UI: server, secret) and every person's connection must be entered again. Servers sealed under the lost key fail with an internal error until then.

**3. Quick start: the `secrets` volume was lost, `pgdata` kept.** `init` generates a new key and a new database password; Postgres still has the old password, so the app cannot connect. Set the database password to the new one, then continue as in case 2:

```sh
docker compose -p mcp-registry exec -T db sh -c \
  'printf "alter user mcp password '\''%s'\'';\n" "$(cat /secrets/pg_password)" | psql -U mcp -d mcp'
docker compose -p mcp-registry -f quickstart.compose.yml run --rm app node dist/cli.js reset-key-check --yes
```

`alter user` prints `ALTER ROLE`; the app then connects, and the second command ends the key mismatch loop. Do not use `down -v`: it deletes the data too.

### Things to know

- **OAuth providers must be on HTTPS and on the same registrable domain as the gateway** (e.g. `git.example.com` and `mcp.example.com`). The session cookie is `SameSite=Strict`, so the browser does not send it on the redirect back from a provider on another site, and the connection fails with `error=unauthenticated`.
- **A wrong OAuth client secret** does not break people's connections, but once their access tokens expire every call to that server fails as "unavailable" (`transport_error` in the call log) until the secret is fixed. Nobody has to reconnect. When rotating a client secret, update it in the gateway first, then revoke the old one at the provider.
- **Search threshold.** `SEARCH_MIN_SIMILARITY` (default `0.83`, calibrated for the bundled model) is the floor for vector matches. Lower it (e.g. `0.80`) if the agent misses obvious tools, raise it if it finds unrelated ones. A restart is enough; no reindex. Recalibrate if you switch to another embedding model.
- **Large tool outputs.** Claude Code truncates MCP tool output at about 25k tokens (`MAX_MCP_OUTPUT_TOKENS`); raise it on the client (`MAX_MCP_OUTPUT_TOKENS=100000 claude`) for large results. The gateway's 20 MiB `UPSTREAM_MAX_RESPONSE_BYTES` only protects server memory.
- **Scope.** Streamable HTTP upstreams only (no stdio, no legacy SSE). Tools only: MCP resources and prompts are not proxied.
- Call arguments and results are never logged. Daily limits reset at midnight UTC.

## Versioning and upgrades

Releases follow [semantic versioning](https://semver.org); changes are listed in [CHANGELOG.md](./CHANGELOG.md). The version is shown by `node dist/cli.js --version`, under your name in the web UI sidebar, in the `serverInfo` that `/mcp` reports to MCP clients, and in the `version` field of the `starting` and `listening` log records.

### What stays stable within 1.x

- **Environment variables:** names and meaning. New ones may be added. The defaults of tuning values (`SEARCH_MIN_SIMILARITY`, `SYNC_INTERVAL_HOURS`, `CALL_LOG_RETENTION_DAYS`, `UPSTREAM_MAX_RESPONSE_BYTES`, `MCP_MAX_BODY_BYTES`) may change in a minor release, with a changelog entry. Embeddings stay 384-dimensional for all of 1.x.
- **CLI:** command names, arguments and exit codes (0 success, 1 operation failed, 2 bad invocation). Output text is for people and may change, except that `create-token` prints the token on the first line of stdout (the second line is the `claude mcp add` command, the reminder goes to stderr) and `login-link` prints the link as the only line on stdout.
- **`/mcp`:** Streamable HTTP at `/mcp` with `Authorization: Bearer sk_...`; the four tools `list_servers`, `search_tools`, `describe_tool`, `call_tool`; their input and output fields are not removed or changed in meaning (new optional fields may appear); the `<server>.<tool>` ref format.
- **URLs registered outside the gateway:** `<PUBLIC_URL>/api/auth/callback` and `<PUBLIC_URL>/api/connections/callback`; `/healthz` answers 200 with the body `ok`.
- **Data:** a database from 0.1.0 or from any earlier 1.x is upgraded automatically at startup; existing `sk_` tokens, sessions and connections keep working across upgrades. The tables themselves are internal: do not read or write them.
- **Deployment names that the documented commands rely on:** in `quickstart.compose.yml` the project name `mcp-registry`, the services `app`, `db`, `init` and the volumes `secrets` and `pgdata`; in the image, port 3000 and `node dist/cli.js`.

Not covered: the rest of `/api/**` (the web client's internal interface), and the shape and text of log records. Agents use `/mcp`, scripts use the CLI; for automation use `login-link`, not log parsing (the `grep "one-time link"` above is a convenience for a person).

### Image tags

`ghcr.io/ai-pipes/mcp-registry` is published for tagged releases only, and only after the release workflow has tested the image. `1.0.0` is exactly that release; `1.0` and `1` point to the latest release of their line and `latest` to the latest release overall. The workflow sets them from the tag that was pushed last, not from the highest version, so releasing an older patch after a newer one moves them back. `quickstart.compose.yml` uses `:1`. To control when you upgrade, pin an exact tag: `MCP_REGISTRY_IMAGE=ghcr.io/ai-pipes/mcp-registry:1.0.0`.

### Upgrading

There are no down migrations, so a backup is the way back. The compose commands below (backup, `up -d`, restore) were run, with a different project name, against stacks of both layouts. `pull` was run only against a locally built image served from a local registry, since no `:1` image is published yet, and `git fetch --tags && git checkout v<version>` was not run. In the commands, `docker compose` is the one you already use for the stack (the quick start needs `-p mcp-registry`, plus the same `--env-file` or environment as before).

1. **Back up the database and the key.** Source layout (`docker-compose.yml` + `.env`):

   ```sh
   docker compose exec -T db pg_dump -Fc -U mcp -d mcp > mcp-$(date +%F).dump
   cp .env .env.backup                       # holds SECRETS_KEY
   ```

   Quick start (the key is the `secrets_key` file in the `secrets` volume):

   ```sh
   docker compose -p mcp-registry exec -T db pg_dump -Fc -U mcp -d mcp > mcp-$(date +%F).dump
   (umask 077; docker compose -p mcp-registry exec -T app cat /secrets/secrets_key > secrets_key.backup)
   ```

   Keep the dump and the key together: a database restored without its key has no usable secrets.

2. **Upgrade.** Source layout: check out the release tag and rebuild.

   ```sh
   git fetch --tags && git checkout v<version>
   docker compose up -d --build
   ```

   Quick start: replace your saved `quickstart.compose.yml` with the one from the release tag, then pull and start with the same `--env-file` or environment as before. The `--env-file my.env` flag in the commands below is needed only if you started the stack with a settings file; without one, run the same commands without it. `up -d` alone does not pull a tag such as `:1` that is already cached; `pull` fetches the new image and `up -d` then recreates the app.

   ```sh
   docker compose -p mcp-registry --env-file my.env -f quickstart.compose.yml pull
   docker compose -p mcp-registry --env-file my.env -f quickstart.compose.yml up -d
   ```

3. **Start with the key you have been using.** The first start with a version that has the key check records it; a different key later stops the server (see "Recovering from a key mismatch"). Migrations run at startup under a lock; check `docker compose logs app` for the `starting` record with the new version and for `listening`.

### Going back

A database that a newer version has migrated is refused by an older one: the server logs `the database was migrated by a newer version of mcp-registry (unknown migration: ...)` and exits with code 1 (Compose keeps restarting it; the CLI fails the same way). Going back means restoring the dump taken before the upgrade, which discards everything written since. Stop the app, recreate the database, restore, start the old version (use the `docker compose` of your layout as above):

```sh
docker compose stop app
docker compose exec -T db psql -U mcp -d postgres -c 'drop database mcp with (force)' -c 'create database mcp owner mcp'
docker compose exec -T db pg_restore -U mcp -d mcp --exit-on-error < mcp-2026-11-17.dump
```

Then start the old version: source layout, check out the old tag and `docker compose up -d --build`; quick start, `MCP_REGISTRY_IMAGE=ghcr.io/ai-pipes/mcp-registry:<old version> docker compose -p mcp-registry --env-file my.env -f quickstart.compose.yml up -d` with the old release's compose file (drop `--env-file my.env` if you do not use a settings file). Put the key from the backup back if you rotated it after the dump was taken.

## Development

Requires Node 22, pnpm (version pinned in `package.json`) and Docker (integration tests start Postgres with Testcontainers).

```sh
pnpm install
pnpm verify                  # lint, format, types, architecture rules, knip, unit + integration tests, UI build
bash scripts/smoke.sh        # end-to-end check of the built image: CLI -> MCP search -> describe -> call
bash scripts/e2e-image.sh    # Playwright against the built image (pnpm exec playwright install chromium)
pnpm dev:web                 # UI dev server, proxies /api and /mcp to localhost:3000
```

Architecture, layer rules and conventions: [CONTRIBUTING.md](./CONTRIBUTING.md) and [CLAUDE.md](./CLAUDE.md). Security reports: [SECURITY.md](./SECURITY.md).

## License

[MIT](./LICENSE)
