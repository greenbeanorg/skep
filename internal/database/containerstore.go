package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// ContainerStore reads and writes containers (ADR 0010 decision 8). The
// desired columns (hostname, config, desired_state) are written by the
// container tasks acting for a user; the observed_* columns are what the
// agent saw on the host. started_once only ever goes from 0 to 1 (the
// schema's trigger enforces it).
type ContainerStore struct {
	db *sql.DB
}

// NewContainerStore returns the container store for the agent database.
func NewContainerStore(db *sql.DB) *ContainerStore { return &ContainerStore{db: db} }

// containerConfig is the JSON of containers.config.
type containerConfig struct {
	Options          models.ContainerOptions `json:"options"`
	SSHAuthorizedKey string                  `json:"ssh_authorized_key,omitempty"`
}

func configJSON(c models.Container) (string, error) {
	raw, err := json.Marshal(containerConfig{Options: c.Options, SSHAuthorizedKey: c.SSHAuthorizedKey})
	return string(raw), err
}

// InsertTx stores a new, unprovisioned container in the transaction that
// creates its task. A name another container has is container_name_taken;
// the pool is the template's.
func (s *ContainerStore) InsertTx(ctx context.Context, tx *sql.Tx, nodeID string, c models.Container) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM containers WHERE name = ?`, c.Name).Scan(&n); err != nil {
		return fmt.Errorf("database: check container name: %w", err)
	}
	if n > 0 {
		return &models.Error{Code: models.CodeContainerNameTaken, Message: "another container already has this name"}
	}
	var pool string
	err := tx.QueryRowContext(ctx, `SELECT storage_pool_id FROM container_templates WHERE id = ?`, c.TemplateID).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("template %q: %w", c.TemplateID, models.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("database: read the container's template: %w", err)
	}
	cfg, err := configJSON(c)
	if err != nil {
		return err
	}
	at := formatTime(c.CreatedAt)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO containers (id, node_id, name, kind, hostname, vnet, config, desired_state, observed_state,
			template_id, storage_pool_id, provisioned, started_once, created_at, updated_at)
		VALUES (?, ?, ?, 'jail', ?, 1, ?, 'stopped', 'unknown', ?, ?, 0, 0, ?, ?)`,
		c.ID, nodeID, c.Name, c.Hostname, cfg, c.TemplateID, pool, at, at); err != nil {
		return fmt.Errorf("database: insert container: %w", err)
	}
	return nil
}

const containerColumns = `c.id, c.name, c.hostname, c.config, c.template_id, t.name, p.name,
	COALESCE(t.observed_release, ''), c.provisioned, c.started_once, c.desired_state, c.observed_state,
	c.observed_jid, COALESCE(c.observed_detail, ''), c.observed_at, c.created_at, c.updated_at`

const containerFrom = ` FROM containers c
	JOIN container_templates t ON t.id = c.template_id
	JOIN storage_pools p ON p.id = c.storage_pool_id`

func scanContainer(row rowScanner) (models.Container, error) {
	var (
		c                        models.Container
		cfg                      string
		provisioned, startedOnce int
		jid                      sql.NullInt64
		observedAt               sql.NullString
		created, updated         string
	)
	if err := row.Scan(&c.ID, &c.Name, &c.Hostname, &cfg, &c.TemplateID, &c.TemplateName, &c.Pool, &c.Release,
		&provisioned, &startedOnce, &c.DesiredState, &c.State, &jid, &c.StateDetail, &observedAt, &created, &updated); err != nil {
		return models.Container{}, err
	}
	var conf containerConfig
	if err := json.Unmarshal([]byte(cfg), &conf); err != nil {
		return models.Container{}, fmt.Errorf("database: container config: %w", err)
	}
	c.Options, c.SSHAuthorizedKey = conf.Options, conf.SSHAuthorizedKey
	c.Provisioned, c.StartedOnce = provisioned == 1, startedOnce == 1
	if jid.Valid {
		v := int(jid.Int64)
		c.JID = &v
	}
	var err error
	if observedAt.Valid {
		at, perr := parseTime(observedAt.String)
		if perr != nil {
			return models.Container{}, perr
		}
		c.ObservedAt = &at
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return models.Container{}, err
	}
	c.UpdatedAt, err = parseTime(updated)
	return c, err
}

// Get returns one container, or models.ErrNotFound.
func (s *ContainerStore) Get(ctx context.Context, id string) (models.Container, error) {
	c, err := scanContainer(s.db.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Container{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	if err != nil {
		return models.Container{}, fmt.Errorf("database: get container: %w", err)
	}
	return c, nil
}

// List returns a page of containers ordered by name, and the cursor of the
// next page.
func (s *ContainerStore) List(ctx context.Context, limit int, cursor string) ([]models.Container, string, error) {
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) == 0 {
			return nil, "", models.Invalid("invalid cursor")
		}
		after = string(b)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+containerColumns+containerFrom+
		` WHERE c.name > ? ORDER BY c.name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("database: list containers: %w", err)
	}
	list := []models.Container{}
	for rows.Next() {
		c, err := scanContainer(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("database: list containers: %w", err)
		}
		list = append(list, c)
	}
	if err := closeRows(rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > limit {
		list = list[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(list[limit-1].Name))
	}
	return list, next, nil
}

// IDs lists every container's ID.
func (s *ContainerStore) IDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM containers ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("database: list container IDs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, closeRows(rows)
}

func (s *ContainerStore) exec(ctx context.Context, what, id, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("database: %s: %w", what, err)
	}
	return oneRow(res, "container", id)
}

// SetProvisioned records a finished container.create.
func (s *ContainerStore) SetProvisioned(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, "provision container", id,
		`UPDATE containers SET provisioned = 1, observed_state = 'stopped', observed_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at), formatTime(at), id)
}

// MarkStarting records, before jail(8) runs, that the jail is about to
// run code for the first time or again: started_once becomes 1 for good
// and the desired state running (decision 13: from here on Skep never
// writes into the tree).
func (s *ContainerStore) MarkStarting(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, "mark container starting", id,
		`UPDATE containers SET started_once = 1, desired_state = 'running', observed_state = 'starting', observed_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at), formatTime(at), id)
}

// SetDesired records the desired state a user asked for.
func (s *ContainerStore) SetDesired(ctx context.Context, id, state string, at time.Time) error {
	return s.exec(ctx, "set container desired state", id,
		`UPDATE containers SET desired_state = ?, updated_at = ? WHERE id = ?`, state, formatTime(at), id)
}

// SaveObserved records what the agent saw on the host. It writes observed
// columns only.
func (s *ContainerStore) SaveObserved(ctx context.Context, id, state string, jid *int, detail string, at time.Time) error {
	var j, d any
	if jid != nil {
		j = *jid
	}
	if detail != "" {
		d = detail
	}
	return s.exec(ctx, "save observed container", id,
		`UPDATE containers SET observed_state = ?, observed_jid = ?, observed_detail = ?, observed_at = ? WHERE id = ?`,
		state, j, d, formatTime(at), id)
}

// UpdateConfig records a changed hostname, options, or SSH key.
func (s *ContainerStore) UpdateConfig(ctx context.Context, c models.Container, at time.Time) error {
	cfg, err := configJSON(c)
	if err != nil {
		return err
	}
	return s.exec(ctx, "update container", c.ID,
		`UPDATE containers SET hostname = ?, config = ?, updated_at = ? WHERE id = ?`, c.Hostname, cfg, formatTime(at), c.ID)
}

// Delete removes a container's row and, by trigger, its snapshot rows.
func (s *ContainerStore) Delete(ctx context.Context, id string) error {
	return s.exec(ctx, "delete container", id, `DELETE FROM containers WHERE id = ?`, id)
}
