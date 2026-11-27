// Package migrations embeds the versioned SQL files that sit next to it. They are applied at start
// under an advisory lock and recorded by file name in platform.schema_migrations, so the files of
// the 1.x releases (0001-0010) keep their names and bytes: a database created by 1.x sees nothing
// new. A schema change is a new file; an applied one is never edited.
package migrations

import "embed"

// FS holds every migration file at its root.
//
//go:embed *.sql
var FS embed.FS
