// Package migrations embeds the versioned SQL files that sit next to it, so the Go server applies
// the same migrations as the TypeScript one.
package migrations

import "embed"

// FS holds every migration file at its root.
//
//go:embed *.sql
var FS embed.FS
