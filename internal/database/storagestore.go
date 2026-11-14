package database

import (
	"context"
	"database/sql"
	"fmt"
)

// StorageStore reads what the storage inventory needs from agent.db: which
// resources exist, so a dataset can be matched to its owner.
type StorageStore struct {
	db *sql.DB
}

// NewStorageStore returns a StorageStore on agent.db.
func NewStorageStore(db *sql.DB) *StorageStore { return &StorageStore{db: db} }

// Owners is the set of resource IDs that can own datasets.
type Owners struct {
	VMs        map[string]bool
	Containers map[string]bool
	Templates  map[string]bool
}

// Owners returns the ID of every VM, jail, and template.
func (s *StorageStore) Owners(ctx context.Context) (Owners, error) {
	out := Owners{}
	for _, q := range []struct {
		table string
		dst   *map[string]bool
	}{
		{"virtual_machines", &out.VMs},
		{"containers", &out.Containers},
		{"container_templates", &out.Templates},
	} {
		set, err := s.ids(ctx, q.table)
		if err != nil {
			return Owners{}, err
		}
		*q.dst = set
	}
	return out, nil
}

// ids reads the IDs of one table. The table name is one of the constants
// above, never input.
func (s *StorageStore) ids(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM `+table)
	if err != nil {
		return nil, fmt.Errorf("database: list %s IDs: %w", table, err)
	}
	set := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		set[id] = true
	}
	return set, closeRows(rows)
}
