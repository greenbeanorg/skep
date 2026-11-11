// Package database opens the local SQLite node database and applies
// versioned schema migrations.
//
// SQLite holds local node state only; it is not the cluster database. Other
// packages should depend on small interfaces rather than on *sql.DB so the
// cluster layer can use a different store later.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	// Pure-Go SQLite driver: no cgo, so the FreeBSD cross-compile keeps
	// working. See docs/decisions/0001-go-toolchain-and-sqlite-driver.md.
	_ "modernc.org/sqlite"
)

// Open opens (creating if necessary) the SQLite database at path.
//
// The parent directory is created with mode 0700 and the database file with
// mode 0600: it will hold password hashes and audit data. Foreign keys are
// enforced, the journal runs in WAL mode, and writers wait up to five
// seconds for a lock.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database: path must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database: resolve %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("database: create directory: %w", err)
	}
	// Create the file ourselves so its permissions never depend on umask
	// defaults inside the driver. An existing file is left untouched.
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("database: create %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("database: create %s: %w", abs, err)
	}

	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + (&url.URL{Path: abs}).EscapedPath() + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", abs, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: open %s: %w", abs, err)
	}
	return db, nil
}
