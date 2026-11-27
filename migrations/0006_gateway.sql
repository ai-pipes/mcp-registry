create schema if not exists gateway;

-- One row per call_tool: who, with which token, what, when, how long, how it ended. Never
-- arguments or results. `user_id`/`token_id` are identity ids and `server` a catalog
-- slug: no foreign keys across modules. Kept CALL_LOG_RETENTION_DAYS (scheduler).
create table gateway.calls (
  id bigserial primary key,
  user_id uuid not null,
  token_id uuid,
  server text not null,
  tool text not null,
  started_at timestamptz not null,
  duration_ms integer not null check (duration_ms >= 0),
  outcome text not null check (outcome in (
    'ok', 'upstream_error', 'timeout', 'transport_error', 'denied:not_found',
    'denied:invalid_arguments', 'denied:quota', 'denied:needs_connection', 'denied:expired'
  ))
);

create index calls_started_at_idx on gateway.calls (started_at);

-- Calls counted against a server's daily limit, per person and UTC day (gateway/domain QuotaDay).
create table gateway.quota (
  user_id uuid not null,
  server text not null,
  day date not null,
  count integer not null check (count >= 0),
  primary key (user_id, server, day)
);
