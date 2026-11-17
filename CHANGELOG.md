# Changelog

All notable changes to this project are documented in this file. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [semantic versioning](https://semver.org). What stays stable within 1.x is described in the README, section "Versioning and upgrades".

## [1.0.0] - Unreleased

Upgrading from 0.1.0: back up the database and `SECRETS_KEY`, replace the image, start. Two migrations (`0009`, `0010`) run at startup. The first start records a check value for the configured `SECRETS_KEY` (see "Added"), so start with the key you have been using. If you set `PGSSLMODE`, read the entry under "Changed".

### Added

- Sign in without an OIDC provider. With none of `OIDC_ISSUER`, `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` set, people sign in to the UI with one-time links (valid 15 minutes, used once). On the first start with an empty database the server creates the admin `local-admin` and logs one link at `warn` level; a new link is logged on each start until that admin has signed in once. New CLI command `login-link <sub>` prints a link for an existing person; it also works with OIDC configured, as emergency access. See [SECURITY.md](SECURITY.md#running-without-oidc).
- `SECRETS_KEY_FILE` and `PGPASSWORD_FILE` read the secrets key and the database password from files (Docker secrets, generated volumes). A variable together with its `_FILE` twin, or an unreadable or empty file, is a startup error that names the variable and the path, never the content.
- Secrets key check. The database keeps a small value sealed with the key recorded on the first start with this version (also the first start after upgrading from 0.1.0). On every start the server compares the configured key with it: it continues if the key opens it, warns while only `SECRETS_KEY_OLD` does, and otherwise logs one fatal record and exits with code 1 instead of running with secrets it cannot read. CLI commands warn on a mismatch, and `create-server` is refused. New CLI command `reset-key-check --yes` removes the check so the next start records the configured key; the README section "Recovering from a key mismatch" describes when to use it.
- `quickstart.compose.yml`: a Compose file that starts the gateway and Postgres with no settings and no `.env`. A one-shot service generates the secrets key and the database password once into a `secrets` volume. The port is published on `127.0.0.1` by default (`BIND_ADDRESS`); optional settings pass through from the environment. The default image tag is published with the 1.0.0 release; until then set `MCP_REGISTRY_IMAGE`.
- The gateway version: `node dist/cli.js --version`, a line under the person's name in the web UI sidebar, a `version` field in the `listening` log record, and `GET /api/version` (session required).

### Changed

- `OIDC_ISSUER`, `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` are optional. Set all three or none; a partial set stops the server at startup and names the missing ones. Installations that already set all three behave as before.
- `rotate-secrets-key` prints an extra `key check:` line (`re-sealed`, `already current`, `none stored` or `cannot be read with either key`) and exits with code 1 if the check value cannot be read with either key. It also re-seals the check value.
- The `postgres` client is upgraded from 3.4.9 to 4.1.1. The client now reads `PGSSLMODE`; 3.x ignored it. An installation that sets `PGSSLMODE` may now be required to use TLS when connecting to the database: `require` uses TLS and fails against a server without it, and `verify-ca` and `verify-full` check the certificate against Node's CA store. A TLS setting in `DATABASE_URL` still takes precedence. If the database connection fails after the upgrade, check `PGSSLMODE` first. The bundled Compose files do not set it.
- Other dependency updates (`hono`, `@tanstack/react-query`, `tailwindcss`, `cn`, and the GitHub Actions used for builds).

### Fixed

- The call log filters on the admin page (server and outcome) violated the strict Content Security Policy in WebKit browsers (Safari). They now use the same select component as the rest of the UI.

### Security

- A key that does not match the database is detected at startup instead of failing later on each call with secrets sealed under another key (see "Added").
- One-time sign-in links are stored only as a SHA-256 hash, are removed when used, and failed attempts are rate-limited per client IP. While the first-start link is valid, anyone who can read the container log can use it; see [SECURITY.md](SECURITY.md#running-without-oidc).

## [0.1.0] - 2026-10-30

Initial public release.

- Four meta-tools on `/mcp` (`list_servers`, `search_tools`, `describe_tool`, `call_tool`) instead of the upstream tools; hybrid search (full-text and vector) with a similarity floor.
- Web UI login through any OIDC provider that supplies groups or roles; personal API tokens (`sk_...`) for `/mcp`.
- Server catalog with group access, hidden tools, call timeouts and per-person daily limits; synchronisation with upstream every 6 hours and on demand.
- Per-user credentials (OAuth 2.0 Authorization Code with PKCE and automatic refresh, or personal access tokens) or one shared secret per server.
- Secrets encrypted at rest with AES-256-GCM; key rotation without downtime.
- Local embeddings (`multilingual-e5-small`, baked into the image) or an OpenAI-compatible embeddings API.
- Call log and audit log of admin actions; strict CSP web UI.
