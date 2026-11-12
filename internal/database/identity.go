package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureLocalNode returns the stable ID of the node this database belongs
// to. On first start it generates a random UUID and stores it in the nodes
// table; every later start returns the same ID, even when the hostname has
// changed. The name is only used when the row is created.
func EnsureLocalNode(ctx context.Context, db *sql.DB, name string) (id string, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("database: node identity: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	err = tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE is_local = 1`).Scan(&id)
	switch {
	case err == nil:
		return id, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("database: node identity: %w", err)
	}

	if id, err = newUUID(); err != nil {
		return "", fmt.Errorf("database: node identity: %w", err)
	}
	if name == "" {
		name = "node-" + id[:8]
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO nodes (id, name, is_local) VALUES (?, ?, 1)`, id, name); err != nil {
		return "", fmt.Errorf("database: node identity: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("database: node identity: %w", err)
	}
	return id, nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
