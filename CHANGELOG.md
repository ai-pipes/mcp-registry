# Changelog

All notable changes to this project are documented in this file. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [semantic versioning](https://semver.org). What stays stable within 1.x is described in the README, section "Versioning and upgrades".

## [1.0.1] - 2026-11-20

Patch release, no migrations. After you change a server's address or OAuth endpoints you enter its secret again (see "Security").

### Fixed

- The audit page (`GET /api/audit`) sorted entries by the text of their id, so entry 99 appeared above entry 100 and paging with `before` skipped entries. It now sorts by the numeric id, newest first.
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

[1.0.1]: https://github.com/ai-pipes/mcp-registry/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/ai-pipes/mcp-registry/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/ai-pipes/mcp-registry/releases/tag/v0.1.0
