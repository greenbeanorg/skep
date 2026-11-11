// Package migrations embeds the versioned SQL schema migrations so every
// Skep binary carries the schema it needs.
//
// There are two independent migration sets, one per database, because the
// privileged agent and the unprivileged API each own a database the other
// cannot open (docs/decisions/0004-privilege-boundary.md).
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/agent/*.sql sqlite/api/*.sql
var files embed.FS

// Agent returns the migrations for agent.db, the node state owned by
// skep-agent, as a flat file system of NNNN_name.sql files.
func Agent() fs.FS { return sub("sqlite/agent") }

// API returns the migrations for api.db, the identity state owned by
// skep-api, as a flat file system of NNNN_name.sql files.
func API() fs.FS { return sub("sqlite/api") }

func sub(dir string) fs.FS {
	fsys, err := fs.Sub(files, dir)
	if err != nil {
		// Unreachable: the directories are embedded at compile time.
		panic("migrations: " + err.Error())
	}
	return fsys
}
