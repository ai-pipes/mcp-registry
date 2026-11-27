-- A known text sealed with SECRETS_KEY: lets the server notice at start that the configured key
-- is not the one the stored secrets were sealed with. One row at most.
create table platform.key_check (
  id         boolean primary key default true check (id),
  sealed     bytea not null,
  created_at timestamptz not null default now()
);
