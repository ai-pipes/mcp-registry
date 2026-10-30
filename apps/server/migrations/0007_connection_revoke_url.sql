-- The OAuth revoke URL in place when a connection's tokens were issued. Disconnect revokes only
-- at this URL. The revoke URL leaves the config fingerprint (`issued_for`), so turning revocation
-- on or changing its URL no longer expires connections.
alter table connections.connections add column revoke_url text;

-- Fingerprints written before this migration were a JSON array of six elements, the revoke URL
-- last. It was the revoke URL at issue time (a connection is bound to its fingerprint), so it
-- moves to `revoke_url`, and the fingerprint drops it: existing connections stay usable.
update connections.connections
set revoke_url = case when kind = 'oauth' then issued_for::json ->> 5 end,
    issued_for = regexp_replace(issued_for, ',(null|"(?:[^"\\]|\\.)*")\]$', ']')
where json_typeof(issued_for::json) = 'array' and json_array_length(issued_for::json) = 6;

update connections.pending
set issued_for = regexp_replace(issued_for, ',(null|"(?:[^"\\]|\\.)*")\]$', ']')
where json_typeof(issued_for::json) = 'array' and json_array_length(issued_for::json) = 6;
