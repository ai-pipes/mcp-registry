// Package search is the public surface of the search module: the tool index and its ranking. The
// composition builds a Service over Deps (the Postgres index and an embedder from infra) and
// registers its EventHandlers with the outbox; the gateway calls SearchTools and the CLI calls
// ReindexAll. Other modules import this package only.
package search
