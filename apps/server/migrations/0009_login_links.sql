create table identity.login_links (
  code_hash  text primary key,
  user_id    uuid not null references identity.users (id) on delete cascade,
  created_at timestamptz not null,
  expires_at timestamptz not null
);
create index login_links_expires_at on identity.login_links (expires_at);
