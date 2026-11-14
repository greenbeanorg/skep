package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// SnapshotStore reads and writes the snapshots table (ADR 0010 decision
// 2d). As everywhere in agent.db, the configuration columns are written
// by tasks acting for a user (InsertTx, SetProvisioned, Delete) and the
// observed_* columns by SaveObserved only.
type SnapshotStore struct {
	db *sql.DB
}

// NewSnapshotStore returns the snapshot store for the agent database.
func NewSnapshotStore(db *sql.DB) *SnapshotStore { return &SnapshotStore{db: db} }

// InsertTx stores a new, unprovisioned snapshot in the transaction that
// creates its task. It refuses a name the resource already uses
// (snapshot_name_taken) and a resource that already has max snapshots
// (snapshot_limit).
func (s *SnapshotStore) InsertTx(ctx context.Context, tx *sql.Tx, snap models.Snapshot, max int) error {
	var n, same int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(name = ?), 0) FROM snapshots WHERE resource_kind = ? AND resource_id = ?`,
		snap.Name, snap.ResourceKind, snap.ResourceID).Scan(&n, &same); err != nil {
		return fmt.Errorf("database: count snapshots: %w", err)
	}
	switch {
	case same > 0:
		return &models.Error{Code: models.CodeSnapshotNameTaken,
			Message: "this " + snap.ResourceKind + " already has a snapshot with this name"}
	case n >= max:
		return &models.Error{Code: models.CodeSnapshotLimit,
			Message: fmt.Sprintf("this %s has %d snapshots, the most storage.max_snapshots_per_resource allows; delete one first", snap.ResourceKind, n),
			Details: map[string]any{"limit": max}}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots (id, resource_kind, resource_id, name, description, consistency, provisioned, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		snap.ID, snap.ResourceKind, snap.ResourceID, snap.Name, snap.Description, snap.Consistency,
		formatTime(snap.CreatedAt)); err != nil {
		return fmt.Errorf("database: insert snapshot: %w", err)
	}
	return nil
}

const snapshotColumns = `id, resource_kind, resource_id, name, description, consistency, provisioned, created_at,
	observed_exists, COALESCE(observed_used_bytes, 0), observed_at`

func scanSnapshot(row rowScanner) (models.Snapshot, error) {
	var (
		snap        models.Snapshot
		provisioned int
		created     string
		exists      sql.NullInt64
		used        int64
		at          sql.NullString
	)
	if err := row.Scan(&snap.ID, &snap.ResourceKind, &snap.ResourceID, &snap.Name, &snap.Description,
		&snap.Consistency, &provisioned, &created, &exists, &used, &at); err != nil {
		return models.Snapshot{}, err
	}
	snap.Provisioned = provisioned == 1
	var err error
	if snap.CreatedAt, err = parseTime(created); err != nil {
		return models.Snapshot{}, err
	}
	if exists.Valid {
		obs := &models.SnapshotObserved{Exists: exists.Int64 == 1, UsedBytes: used}
		if at.Valid {
			t, err := parseTime(at.String)
			if err != nil {
				return models.Snapshot{}, err
			}
			obs.At = &t
		}
		snap.Observed = obs
	}
	snap.Clones = []models.SnapshotClone{}
	return snap, nil
}

// List returns the snapshots of a resource, oldest first, with their
// clones.
func (s *SnapshotStore) List(ctx context.Context, kind, resourceID string) ([]models.Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots
		WHERE resource_kind = ? AND resource_id = ? ORDER BY created_at, id`, kind, resourceID)
	if err != nil {
		return nil, fmt.Errorf("database: list snapshots: %w", err)
	}
	list := []models.Snapshot{}
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("database: list snapshots: %w", err)
		}
		list = append(list, snap)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	return list, s.attachClones(ctx, list)
}

// Get returns one snapshot of a resource, or models.ErrNotFound.
func (s *SnapshotStore) Get(ctx context.Context, kind, resourceID, id string) (models.Snapshot, error) {
	snap, err := scanSnapshot(s.db.QueryRowContext(ctx, `SELECT `+snapshotColumns+` FROM snapshots
		WHERE id = ? AND resource_kind = ? AND resource_id = ?`, id, kind, resourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Snapshot{}, fmt.Errorf("snapshot %q: %w", id, models.ErrNotFound)
	}
	if err != nil {
		return models.Snapshot{}, fmt.Errorf("database: read snapshot: %w", err)
	}
	list := []models.Snapshot{snap}
	if err := s.attachClones(ctx, list); err != nil {
		return models.Snapshot{}, err
	}
	return list[0], nil
}

// attachClones fills in the clones that depend on each snapshot.
func (s *SnapshotStore) attachClones(ctx context.Context, list []models.Snapshot) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	for i, snap := range list {
		ids[i] = snap.ID
	}
	clones, err := s.ClonesOf(ctx, ids)
	if err != nil {
		return err
	}
	for i := range list {
		if c := clones[list[i].ID]; c != nil {
			list[i].Clones = c
		}
	}
	return nil
}

// ClonesOf returns, for each of the given snapshots that has any, the
// clones made from it: the database's view of snapshot_has_clones.
func (s *SnapshotStore) ClonesOf(ctx context.Context, snapshotIDs []string) (map[string][]models.SnapshotClone, error) {
	out := map[string][]models.SnapshotClone{}
	if len(snapshotIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(snapshotIDs)), ", ")
	args := make([]any, 0, 2*len(snapshotIDs))
	for _, id := range snapshotIDs {
		args = append(args, id)
	}
	args = append(args, args...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT cloned_from_snapshot_id, id, name FROM virtual_machines WHERE cloned_from_snapshot_id IN (`+marks+`)
		UNION ALL
		SELECT cloned_from_snapshot_id, id, name FROM containers WHERE cloned_from_snapshot_id IN (`+marks+`)
		ORDER BY 1, 3`, args...)
	if err != nil {
		return nil, fmt.Errorf("database: read clones: %w", err)
	}
	for rows.Next() {
		var snap string
		var c models.SnapshotClone
		if err := rows.Scan(&snap, &c.ID, &c.Name); err != nil {
			rows.Close()
			return nil, err
		}
		out[snap] = append(out[snap], c)
	}
	return out, closeRows(rows)
}

// ResourceClones returns the clones made from any snapshot of a resource.
func (s *SnapshotStore) ResourceClones(ctx context.Context, kind, resourceID string) ([]models.SnapshotClone, error) {
	list, err := s.List(ctx, kind, resourceID)
	if err != nil {
		return nil, err
	}
	var out []models.SnapshotClone
	for _, snap := range list {
		out = append(out, snap.Clones...)
	}
	return out, nil
}

// SetProvisioned records that a snapshot was taken, how consistent it is,
// and what it was observed to hold.
func (s *SnapshotStore) SetProvisioned(ctx context.Context, id, consistency string, usedBytes int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE snapshots SET provisioned = 1, consistency = ?, observed_exists = 1, observed_used_bytes = ?, observed_at = ?
		WHERE id = ?`, consistency, usedBytes, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: mark snapshot provisioned: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("snapshot %q: %w", id, models.ErrNotFound)
	}
	return nil
}

// SaveObserved records what ZFS showed of a snapshot.
func (s *SnapshotStore) SaveObserved(ctx context.Context, id string, exists bool, usedBytes int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE snapshots SET observed_exists = ?, observed_used_bytes = ?, observed_at = ? WHERE id = ?`,
		boolInt(exists), usedBytes, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: save observed snapshot: %w", err)
	}
	return nil
}

// Delete removes snapshot rows. Rows that do not exist are ignored; a row
// a clone depends on is refused by the database.
func (s *SnapshotStore) Delete(ctx context.Context, ids ...string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id); err != nil {
			return fmt.Errorf("database: delete snapshot: %w", err)
		}
	}
	return nil
}

// CloneInProgress reports whether an unfinished vm.clone task is making
// the given VM: such a VM is not provisioned yet and must not be deleted
// under the task.
func (s *SnapshotStore) CloneInProgress(ctx context.Context, targetVMID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks WHERE type = ? AND status IN ('pending', 'running')
		AND json_extract(payload, '$.target_vm_id') = ?`, models.TaskVMClone, targetVMID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("database: look for a clone task: %w", err)
	}
	return n > 0, nil
}
