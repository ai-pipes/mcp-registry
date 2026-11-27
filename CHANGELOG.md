# Changelog

All notable changes to this project are documented in this file. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [semantic versioning](https://semver.org). What stays stable within a major version is described in the README, section "Versioning and upgrades".

## [2.0.0] - 2026-11-27

The gateway is rewritten in Go. Same environment variables, same database (no migration: 1.0.x tables are read as they are, and 1.0.1 can still start on the database afterwards), same `/mcp`, web UI, tokens, sessions and connections. Upgrading: back up the database and the key, change the CLI calls in your scripts (below), replace the image, start. The README section "Upgrading from 1.x to 2.0" walks through it, "Going back" covers 2.0.0 -> 1.0.1. The 1.x line receives no further releases.

### Breaking

- The operator CLI is `mcp-registry <command>` (on the `PATH` of the image): `docker compose exec app mcp-registry login-link <sub>`. `node dist/cli.js <command>` fails, because the image has no Node.
- The image contains no `node`, `curl` or `wget`. A compose `command:` or `healthcheck:` override, or a platform health probe, that runs one of them breaks; the image's own `HEALTHCHECK` runs `mcp-registry healthcheck`.

### Added

- `mcp-registry healthcheck`: exit 0 when `GET /healthz` on `127.0.0.1:$PORT` answers 200, else exit 1 with one short line on stderr. Needs no configuration, no database and no key; the image's `HEALTHCHECK` uses it (every 10 s, 5 s timeout, 30 s start period, 3 retries, as in 1.x).
- `create-user --member` demotes an existing admin (see "Changed").
- The licence texts of everything the image redistributes are in `/usr/share/licenses/mcp-registry/` (`THIRD_PARTY_NOTICES` and `third_party/`).

### Changed

- The quick start's default image is `ghcr.io/ai-pipes/mcp-registry:2` (the compose file of `v2.0.0`). The tag `1` stays on 1.0.1, so an installation on `1` moves to 2.x only when its tag is changed; `latest` moves to 2.0.0.
- `create-user` keeps an existing person's role unless `--admin` (promote) or `--member` (demote) is given; 1.x demoted an existing admin when `--admin` was missing. A new person is still a member without `--admin`.
- `create-server` exits with code 1 when the first sync fails; the server is still created and the output says the sync failed (1.x exited 0).
- `reindex` prints `reindexed <N> tool(s)`, N being the number of indexed tools (1.x printed the same number as `server(s)`). When the embedder fails it prints `error: unexpected failure` and exits 1 instead of a stack trace; the text rows are written either way.
- `/healthz` answers 200 only after the embedding model is loaded (the wait is bounded at 2 minutes, then the start goes on and search stays text-only until it loads, with one `warn` record `search: local embedder unavailable, ...`), so the first search after a start already uses vectors; 1.x answered at once and searched by text alone until its model had loaded. A model that fails to load is retried after a pause (5 s, doubling up to 5 minutes) instead of on every call.
- `EMBEDDER=openai-compatible` without `EMBEDDER_URL` or `EMBEDDER_MODEL` is an ordinary `Invalid configuration:` error naming both variables (1.x crashed with an unhandled exception); `OIDC_SCOPES` is validated only when OIDC is configured.
- The `auth.login` audit record of an OIDC sign-in carries `via: "oidc"` (1.x: empty meta).
- Every response carries `X-Request-Id`, the refusal of a foreign `Host` on `/mcp` included; the `429` for repeated failed token checks on `/mcp` carries `Retry-After`.
- An OAuth callback that arrives under another person's session is refused (`invalid_state`) without spending the starter's pending connect, so the starter can still complete it. A forged OIDC callback no longer breaks a sign-in in progress: the browser's sign-in binding is cleared only by a callback whose `state` matches it.
- Search keeps a tool's stored vector when re-embedding it fails (the text is updated and the vector is re-made later), and a late or repeated "tools removed" event no longer deletes a tool that came back.
- The working directory of the image is `/` (1.x: `/app/apps/server`): relative paths for `create-server --json` or a `*_FILE` variable resolve from `/`.
- The image no longer sets `PORT`, `MODEL_DIR`, `NODE_ENV` or `HF_HUB_OFFLINE` (the defaults are unchanged: port 3000, the model in `/app/models`, no network for the model); it sets `MALLOC_ARENA_MAX=2`. It still runs as uid and gid 1000.
- Outbound TLS trusts the image's system CA store. `NODE_EXTRA_CA_CERTS` has no effect any more; mount a CA bundle and set `SSL_CERT_FILE` (or `SSL_CERT_DIR`).
- On `SIGTERM` the server stops accepting at once, gives calls in flight 5 s to finish (the rest answer `<Server> is unavailable.` and are logged as `transport_error`), cancels a running scheduled sync and exits within 10 s. Both compose files set `stop_grace_period: 15s`; give `docker run` `--stop-timeout 15`.
- `sync`, `create-server`, `reindex`, `rotate-secrets-key` and `reset-key-check` stop cleanly on Ctrl-C or `SIGTERM` (`error: interrupted`, exit 1; each says what was already written) instead of being killed wherever they were; Ctrl-C at the `create-server` secret prompt creates nothing. An unexpected CLI error prints `error: unexpected failure` instead of a stack trace.
- The `SECRETS_KEY_OLD is set` start warning names `mcp-registry rotate-secrets-key`.
- `call_tool` argument validation (advisory: the upstream validates too) uses another JSON Schema validator: refusal messages carry the location (a JSON pointer) and the library's wording but never an argument value; `uniqueItems`, `multipleOf` and some formats are not checked (`pattern` stays unchecked, as in 1.x). A schema that is very large or deep (over 256 KiB, over 100 levels, over 10 000 nodes after expanding references), recursive, or uses `$dynamicRef`/`$recursiveRef`, and arguments over 256 KiB or too costly to check against their schema, are passed on unchecked instead of risking a slow check.
- `/mcp` answers a classic `POST` with `application/json` instead of an SSE stream, runs the requests of a batch one after another (at most 100 per batch) and echoes request ids exactly as the client wrote them.
- Log records have no `pid` and `hostname`; error fields are `{type, label}` without the message (a stack only for panics); a failed start is one `fatal` record and exit code 1.
- Database connection through `pgx`: more `PG*` environment variables are read, without an `sslmode` the connection tries TLS first and falls back (`prefer`), URL options of the 1.x client (`max`, `prepare`, `ssl`, ...) are dropped or translated, and the pool holds 10 connections.
- HTTP: request headers must arrive within 10 s, idle keep-alive connections close after 60 s; paths are not normalised (`/assets/../index.html` is 404), and dotfiles in the web root are not served. `PUBLIC_URL`, `OIDC_ISSUER` and `EMBEDDER_URL` need a host (`http:example.com` or `file:///x` is a configuration error).
- OIDC: the token exchange, key set and UserInfo time out after 10 s (1.x: 30 s); a wrong `iss` on the callback is refused before the code is spent; the ID token needs no `iat` and its `exp` gets no clock tolerance.
- A sync takes at most 10 000 tools from one upstream; an upstream that sends more than 32 requests of its own during one operation fails it.
- The image is about 300 MB (1.x: about 890 MB). Measured on one arm64 machine, it uses less memory and answers concurrent `call_tool` and `search_tools` faster than 1.0.0 (`make load-compare`; CONTRIBUTING.md has the numbers).

### Removed

- The TypeScript server and the Node runtime from the image; `node dist/cli.js`.

### Security

- The OIDC ID token's signature is verified against the provider's key set (1.x did not verify it).
- Secrets, tokens, sign-in codes, keys and sealed values are held in types that print as `[Redacted]` in any log record, error or dump; a repository test fails on a code path that would pass one to a printer or the database driver unrevealed.
- The argument validator's bounds above keep one hostile tool schema or argument from tying up the server, and its refusals never repeat a value the agent sent.
- `call_tool` reads a shared secret together with the server configuration it belongs to, in one read: an administrator's edit between two reads can no longer send a new secret to an old address (or the reverse).

## [1.0.1] - 2026-11-20

Patch release, no migrations. After you change a server's address or OAuth endpoints you enter its secret again (see "Security").

### Fixed

- The audit page (`GET /api/audit`) sorted entries by the text of their id, so entry 99 appeared above entry 100 and paging with `before` skipped entries. It now sorts by the numeric id, newest first.
- The call log page (`GET /api/calls`) broke ties between entries that started at the same moment by the text of their id and, with "Load more", skipped entries that shared the start time of the last entry on the previous page. It now sorts by start time and then by the numeric id, and pages with a (start time, id) cursor (new optional `beforeId` query parameter next to `before`).
- The "Done" button of the new-token dialog did nothing when it was clicked right after the token was created, while the token list was still refreshing. It closes the dialog at once; the token is still shown only once.
- `GET /` is answered with `Cache-Control: no-cache`, like the other pages of the web UI, so a browser does not keep an old page after an upgrade. Files under `/assets/` stay cached for a year.

### Security

- A server's stored secret is cleared when the place it is sent to changes, so an edit can no longer make the gateway send it to a new address. For a `shared_secret` server that is the upstream URL; for an `oauth_user` server (the secret is the OAuth client secret) the token URL, the revoke URL or the client ID. Changing the authorization method already cleared it. Other edits keep the secret. After such a change enter the secret again in the form (it warns before saving and reminds afterwards) or with `PUT /api/servers/:slug/secret`. Until then: for a `shared_secret` server, synchronisation fails with "The server secret is not set" without contacting the upstream, and tool calls to that server fail with "<Server> rejected the gateway credentials. Ask the administrator to check the server key." (the upstream is not called). For an `oauth_user` server the gateway sends no client authentication to the token endpoint, so at a provider that requires the client secret, connecting fails and refreshing a person's token fails; the connection is kept (not marked expired), a still-valid access token keeps working, and once it cannot be used agents see "<Server> is unavailable."
- Anonymous requests can no longer pile up expired pending sign-ins, and failed sign-in callbacks are audited at most 20 times per minute per client address (the sign-in itself is still checked, and a valid one always succeeds; starting a sign-in is not limited). Expired pending sign-ins are now deleted each time a sign-in starts (they used to stay for an hour after their 10 minutes), so that table holds what was started in the last 10 minutes or so; it is bounded by the request rate, not by a cap. Behind a reverse proxy without `TRUST_PROXY` all callers share one address, so audit records of failed sign-ins can stop after 20 in a minute.

## [1.0.0] - 2026-11-17

Upgrading from 0.1.0: back up the database and `SECRETS_KEY`, replace the image, start. Two migrations (`0009`, `0010`) run at startup. The first start records a check value for the configured `SECRETS_KEY` (see "Added"), so start with the key you have been using. If you set `PGSSLMODE`, read the entry under "Changed".

### Added

- Sign in without an OIDC provider. With none of `OIDC_ISSUER`, `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` set, people sign in to the UI with one-time links (valid 15 minutes, used once; stored only as a SHA-256 hash, removed when used, failed attempts rate-limited per client IP). On the first start with an empty database the server creates the admin `local-admin` and logs one link at `warn` level; a new link is logged on each start until that admin has signed in once. New CLI command `login-link <sub>` prints a link for an existing person; it also works with OIDC configured, as emergency access. While the first-start link is valid, anyone who can read the container log can use it; see [SECURITY.md](SECURITY.md#running-without-oidc).
- `SECRETS_KEY_FILE` and `PGPASSWORD_FILE` read the secrets key and the database password from files (Docker secrets, generated volumes). A variable together with its `_FILE` twin, or an unreadable or empty file, is a startup error that names the variable and the path, never the content.
- Secrets key check. The database keeps a small value sealed with the key recorded on the first start with this version (also the first start after upgrading from 0.1.0). On every start the server compares the configured key with it: it continues if the key opens it, warns while only `SECRETS_KEY_OLD` does, and otherwise logs one fatal record and exits with code 1 instead of running with secrets it cannot read, which would otherwise fail later on each call. CLI commands warn on a mismatch, and `create-server` is refused. New CLI command `reset-key-check --yes` removes the check so the next start records the configured key; the README section "Recovering from a key mismatch" describes when to use it.
- `quickstart.compose.yml`: a Compose file that starts the gateway and Postgres with no settings and no `.env`. A one-shot service generates the secrets key and the database password once into a `secrets` volume. The port is published on `127.0.0.1` by default (`BIND_ADDRESS`); optional settings pass through from the environment.
- The gateway version: `node dist/cli.js --version`, a line under the person's name in the web UI sidebar, `GET /api/version` (session required), and the `serverInfo.version` that `/mcp` reports to MCP clients (it was `0.0.0`). Every start now logs `starting` with the `version` field as its first record, so a failed start shows which version failed; `listening` keeps it too.
- A database that was migrated by a newer version is refused: if `platform.schema_migrations` holds a migration this build does not ship, the server logs one fatal record (`the database was migrated by a newer version of mcp-registry (unknown migration: <name>); ...`) and exits with code 1, and the CLI fails the same way, instead of running on a schema it does not know. Going back to an older version means restoring the backup taken before the upgrade; see the README, section "Versioning and upgrades".

### Changed

- `OIDC_ISSUER`, `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` are optional. Set all three or none; a partial set stops the server at startup and names the missing ones. Installations that already set all three behave as before.
- `rotate-secrets-key` prints an extra `key check:` line (`re-sealed`, `already current`, `none stored` or `cannot be read with either key`) and exits with code 1 if the check value cannot be read with either key. It also re-seals the check value.
- The `postgres` client is upgraded from 3.4.9 to 4.1.1. The client now reads `PGSSLMODE`; 3.x ignored it. An installation that sets `PGSSLMODE` may now be required to use TLS when connecting to the database: `require` uses TLS and fails against a server without it, and `verify-ca` and `verify-full` check the certificate against Node's CA store and also the host name, except when the root certificate file is given as `sslrootcert` in `DATABASE_URL`: then `verify-ca` does not check the host name. `PGSSLROOTCERT` and `~/.postgresql/root.crt` are not read: a private CA goes into `DATABASE_URL` as `?sslrootcert=<path>`. A TLS setting in `DATABASE_URL` still takes precedence. If the database connection fails after the upgrade, check `PGSSLMODE` first. The bundled Compose files do not set it.
- Release images are built and tested (version, quick-start smoke test, compose smoke test) before they are published to GHCR.
- Other dependency updates (`hono`, `@tanstack/react-query`, `tailwindcss`, `cn`, and the GitHub Actions used for builds).

### Security

- `@hono/node-server` is updated from 2.1.5 to 2.1.8, which fixes GHSA-qv5m-9pm6-hqp3 (an unhandled `ECONNRESET` during an asynchronous WebSocket upgrade could crash the process) and GHSA-jpvv-r467-5p4w (a backslash in the request path was parsed as a path separator, bypassing path-based access control at a reverse proxy).

### Fixed

- The call log filters on the admin page (server and outcome) violated the strict Content Security Policy in WebKit browsers (Safari). They now use the same select component as the rest of the UI.

## [0.1.0] - 2026-10-30

Initial public release.

- Four meta-tools on `/mcp` (`list_servers`, `search_tools`, `describe_tool`, `call_tool`) instead of the upstream tools; hybrid search (full-text and vector) with a similarity floor.
- Web UI login through any OIDC provider that supplies groups or roles; personal API tokens (`sk_...`) for `/mcp`.
- Server catalog with group access, hidden tools, call timeouts and per-person daily limits; synchronisation with upstream every 6 hours and on demand.
- Per-user credentials (OAuth 2.0 Authorization Code with PKCE and automatic refresh, or personal access tokens) or one shared secret per server.
- Secrets encrypted at rest with AES-256-GCM; key rotation without downtime.
- Local embeddings (`multilingual-e5-small`, baked into the image) or an OpenAI-compatible embeddings API.
- Call log and audit log of admin actions; strict CSP web UI.

[2.0.0]: https://github.com/ai-pipes/mcp-registry/compare/v1.0.1...v2.0.0
[1.0.1]: https://github.com/ai-pipes/mcp-registry/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/ai-pipes/mcp-registry/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/ai-pipes/mcp-registry/releases/tag/v0.1.0
