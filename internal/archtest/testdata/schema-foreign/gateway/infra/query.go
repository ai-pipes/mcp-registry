package infra

// Fixture: the gateway module reads a table of the identity schema.
const listCalls = `select c.id from gateway.calls c join identity.users u on u.id = c.user_id`
