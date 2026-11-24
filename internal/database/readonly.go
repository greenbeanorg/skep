package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// ErrNotInUse is returned by OpenReadOnly when the database has no
// write-ahead log files, which means no service has it open.
var ErrNotInUse = errors.New("database: no service has this database open")

// OpenReadOnly opens an existing database that a running service is using,
// without being able to write to it. It never creates or migrates anything.
//
// It refuses a database without its -wal and -shm files, because SQLite
// would create them even for a read-only connection and leave them behind.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("database: resolve %s: %w", path, err)
	}
	for _, name := range []string{abs, abs + "-wal", abs + "-shm"} {
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			if name == abs {
				return nil, fmt.Errorf("there is no database at %s", abs)
			}
			return nil, fmt.Errorf("%w: %s does not exist", ErrNotInUse, name)
		} else if err != nil {
			return nil, fmt.Errorf("database: %w", err)
		}
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: abs}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("database: open %s: %w", abs, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: open %s: %w", abs, err)
	}
	return db, nil
}
