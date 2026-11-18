package infra

// Fixture: the module's own schema and the cross-cutting platform schema are allowed.
const listCalls = `select c.id from gateway.calls c` + " join gateway.quota q on q.user_id = c.user_id"

const enqueue = `insert into platform.outbox (id) values ($1)`
