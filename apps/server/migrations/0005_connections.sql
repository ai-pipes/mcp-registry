create schema if not exists connections;

-- A person's credentials to one upstream server. `user_id` is an identity user id and `server`
-- a catalog slug: no foreign keys across modules. Tokens are AES-256-GCM sealed (SECRETS_KEY).
create table connections.connections (
  user_id uuid not null,
  server text not null,
  kind text not null check (kind in ('oauth', 'pat')),
  access bytea not null,
  refresh bytea,
  expires_at timestamptz,
  status text not null check (status in ('ready', 'expired')),
  -- The server config (URL, auth kind, OAuth provider) the credential was issued for: when an
  -- admin changes it, the token must not follow to the new place (connections/domain).
  issued_for text not null,
  updated_at timestamptz not null,
  primary key (user_id, server)
);

-- A started OAuth connect (state → PKCE verifier). One-time, short-lived, bound to its person.
create table connections.pending (
  state text primary key,
  user_id uuid not null,
  server text not null,
  code_verifier text not null,
  issued_for text not null,
  expires_at timestamptz not null
);

create index pending_expires_idx on connections.pending (expires_at);
