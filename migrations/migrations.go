// Package migrations embeds the versioned SQL schema migrations so every
// Skep binary carries the schema it needs.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/*.sql
var files embed.FS

// SQLite returns the SQLite migrations as a flat file system of
// NNNN_name.sql files.
func SQLite() fs.FS {
	sub, err := fs.Sub(files, "sqlite")
	if err != nil {
		// Unreachable: the directory is embedded at compile time.
		panic("migrations: " + err.Error())
	}
	return sub
}
