-- Optimistic concurrency for catalog.servers: every edit of a server (fields, status, secret)
-- writes only if the row is still at the version it was read at, and bumps it. A write based on
-- a stale read is refused (server_conflict) instead of silently undoing or mixing another
-- admin's change. Synchronisation state and the key-rotation re-seal (compare-and-set on the
-- secret itself) do not take part.
alter table catalog.servers add column version integer not null default 0;
