package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// Migration is one versioned schema change.
type Migration struct {
	Version  int
	Name     string
	Checksum string
	sql      string
}

var migrationFileRE = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT`

// LoadMigrations reads NNNN_name.sql files from fsys. Versions must start at
// 1 and be contiguous, so a missing or duplicated file is caught at startup.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: read directory: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations: %q is not named NNNN_name.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     m[2],
			Checksum: hex.EncodeToString(sum[:]),
			sql:      string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations: expected version %d, found %d (%s); versions must be contiguous from 1", i+1, m.Version, m.Name)
		}
	}
	return out, nil
}

// Migrate applies every migration in fsys that the database has not seen,
// in order, each in its own transaction. It returns the migrations applied
// by this call.
//
// It refuses to continue when an applied migration was edited afterwards
// (checksum mismatch) or when the database is newer than this binary.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) ([]Migration, error) {
	known, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("migrations: create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	for version, a := range applied {
		if version > len(known) {
			return nil, fmt.Errorf("migrations: database is at version %d (%s) but this binary only knows %d; refusing to run against a newer schema", version, a.Name, len(known))
		}
		if k := known[version-1]; k.Checksum != a.Checksum {
			return nil, fmt.Errorf("migrations: version %d (%s) was changed after it was applied; add a new migration instead of editing an old one", version, k.Name)
		}
	}

	var done []Migration
	for _, m := range known {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// SchemaVersion returns the highest applied migration version, or 0.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("migrations: read schema version: %w", err)
	}
	return int(v.Int64), nil
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[int]Migration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrations: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]Migration{}
	for rows.Next() {
		var m Migration
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum); err != nil {
			return nil, fmt.Errorf("migrations: read schema_migrations: %w", err)
		}
		out[m.Version] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrations: read schema_migrations: %w", err)
	}
	return out, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrations: begin %04d_%s: %w", m.Version, m.Name, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migrations: apply %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`,
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("migrations: record %04d_%s: %w", m.Version, m.Name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrations: commit %04d_%s: %w", m.Version, m.Name, err)
	}
	return nil
}
