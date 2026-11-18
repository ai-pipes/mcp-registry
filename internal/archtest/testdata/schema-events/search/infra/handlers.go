package infra

// Fixture: outbox event names look like `<module>.<word>` but are not SQL.
var handlers = map[string]string{
	"catalog.tools_changed":  "reindex",
	"catalog.server_changed": "reindex",
}
