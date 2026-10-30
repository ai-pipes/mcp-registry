create schema if not exists identity;

create table identity.users (
  id uuid primary key default gen_random_uuid(),
  oidc_sub text not null unique,
  name text not null,
  email text,
  groups text[] not null default '{}',
  role text not null check (role in ('admin', 'member')),
  disabled boolean not null default false,
  created_at timestamptz not null default now(),
  last_login_at timestamptz
);

create table identity.tokens (
  id uuid primary key default gen_random_uuid(),
  user_id uuid not null references identity.users (id) on delete cascade,
  name text not null,
  prefix text not null,
  hash char(64) not null unique,
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);

create index tokens_user_idx on identity.tokens (user_id);

create table identity.sessions (
  id_hash char(64) primary key,
  user_id uuid not null references identity.users (id) on delete cascade,
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  expires_at timestamptz not null
);

create index sessions_user_idx on identity.sessions (user_id);

-- A started OIDC login (state → PKCE verifier, nonce, where to go after). One-time, short-lived.
create table identity.oidc_pending (
  state text primary key,
  code_verifier text not null,
  nonce text not null,
  return_to text not null,
  expires_at timestamptz not null
);

create index oidc_pending_expires_idx on identity.oidc_pending (expires_at);
