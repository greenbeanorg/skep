package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// NodeStore reads and writes the nodes table.
type NodeStore struct {
	db *sql.DB
}

// NewNodeStore returns the node store for the agent database.
func NewNodeStore(db *sql.DB) *NodeStore { return &NodeStore{db: db} }

// SaveObserved records what discovery last saw on a node. It writes only
// observed_* columns: observed state never overwrites desired state.
func (s *NodeStore) SaveObserved(ctx context.Context, nodeID string, hw models.Hardware) error {
	doc, err := json.Marshal(hw)
	if err != nil {
		return fmt.Errorf("database: encode hardware: %w", err)
	}
	at := hw.CollectedAt
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET
			observed_state = 'online',
			observed_hostname = ?, observed_os_name = ?, observed_os_release = ?,
			observed_arch = ?, observed_hardware = ?, observed_at = ?, updated_at = ?
		WHERE id = ?`,
		hw.Host.Hostname, hw.Host.OS.Name, hw.Host.OS.Release, hw.Host.Architecture,
		string(doc), formatTime(at), formatTime(time.Now()), nodeID)
	if err != nil {
		return fmt.Errorf("database: save observed node state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("database: node %q: %w", nodeID, models.ErrNotFound)
	}
	return nil
}
