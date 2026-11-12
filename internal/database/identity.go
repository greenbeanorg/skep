package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/greenbeanorg/skep/internal/ids"
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

	if id, err = ids.NewUUID(); err != nil {
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
