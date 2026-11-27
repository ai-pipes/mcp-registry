create schema if not exists catalog;

create table catalog.servers (
  slug text primary key,
  name text not null,
  description text not null default '',
  url text not null,
  auth jsonb not null,
  -- AES-256-GCM sealed (SECRETS_KEY): client secret of the OAuth app or the shared key.
  secret bytea,
  access jsonb not null,
  timeout_ms int not null,
  daily_limit int,
  status text not null check (status in ('active', 'disabled')),
  last_sync_at timestamptz,
  last_sync_error text,
  -- The admin whose OAuth token the last synchronisation used (an identity user id; no FK across modules).
  sync_user_id uuid,
  created_at timestamptz not null,
  updated_at timestamptz not null
);

create table catalog.tools (
  server text not null references catalog.servers (slug) on delete cascade,
  name text not null,
  title text,
  description text not null,
  input_schema jsonb not null,
  output_schema jsonb,
  annotations jsonb not null default '{}'::jsonb,
  hidden boolean not null default false,
  removed_at timestamptz,
  synced_at timestamptz not null,
  primary key (server, name)
);
