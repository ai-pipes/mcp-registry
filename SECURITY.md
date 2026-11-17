# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability** (private vulnerability reporting / security advisories). Do not open a public issue for a security problem.

Include what you found, how to reproduce it, the affected version or commit, and the impact you expect. You will get an acknowledgement as soon as a maintainer sees the report; fixes are released as a new version and disclosed in a security advisory once a fix is available.

## Supported versions

Only the latest release receives security fixes. Upgrade before reporting if you can.

## Threat model

mcp-registry is designed for individuals and small teams running it inside a private network, behind a TLS-terminating reverse proxy, with a trusted operator. Within that scope:

- **Stored secrets.** Upstream server secrets (shared keys, OAuth client secrets) and people's OAuth tokens and personal access tokens are encrypted at rest with AES-256-GCM under `SECRETS_KEY`. Anyone who has both the database and the key can read them. Keep the key out of the database backups' storage location and restrict access to the host.
- **No secret leaves the server.** Upstream secrets and personal tokens are never returned to the browser or to MCP clients and never written to logs; admin forms treat them as write-only.
- **No payload logging.** `call_tool` arguments and results are not logged or stored; the call log records who called which tool, when, how long it took and the outcome.
- **API tokens.** Personal `sk_` tokens are stored only as SHA-256 hashes, grant access to `/mcp` only (not the REST API), and are revoked when a person is disabled.
- **Per-call access checks.** Every call re-checks that the person may use the server and the tool right now (server enabled, group access, tool not hidden) before any credential is used.
- **Web UI.** OIDC login with PKCE (or a one-time sign-in link, see below), `SameSite=Strict` session cookies, `Origin` checks on mutations, and a strict Content Security Policy (`default-src 'self'`, no inline scripts or styles).
- **Upstream calls.** Redirects are refused, responses are size-limited, and an OAuth token is never sent to an upstream or token endpoint other than the one it was issued for (a changed server configuration expires the connection).

### Running without OIDC

OIDC is optional. Without `OIDC_ISSUER`, `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET`, people sign in with one-time links (`ll_...`, valid 15 minutes, used once, only the SHA-256 stored). This mode is meant for one person or a small trusted team; OIDC is recommended for a team. Know what it means:

- On the first start without OIDC the server creates the administrator `local-admin` and writes a sign-in link into the container log. After every start the log contains an administrator credential for up to 15 minutes, until `local-admin` has signed in for the first time. Anyone who can read the logs in that window, including a log collector, can become admin. Sign in promptly; each new start invalidates the previous link.
- A sign-in link is as powerful as its owner's session. Whoever holds an unused link becomes that person. Hand links over only through a private channel and do not paste them into chats or tickets. A new link for the same person invalidates the earlier one.
- Links are issued only by someone with access to the container (`node dist/cli.js login-link <sub>`); there is no self-service. The command also works when OIDC is configured, as break-glass access.
- Failed link attempts are limited to 10 per minute per client IP. Behind a reverse proxy set `TRUST_PROXY` (and `TRUST_PROXY_HOPS`); otherwise all clients share one bucket and a single client can temporarily lock link sign-in for everyone.
- There is no group management beyond what `create-user` assigns: admins see every active server, members see servers open to everyone or to the login group.

### Out of scope

- Multi-tenant isolation: all people share one installation and one admin group.
- Hostile admins: an admin can point a server at any URL, read the call log and the audit log, and an operator with host access can read the database and the key.
- Exposure to the public internet without TLS, or without a reverse proxy in front.
- The security of upstream MCP servers and OAuth providers themselves.
