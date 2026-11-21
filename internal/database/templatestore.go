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

// TemplateStore reads and writes container_templates (ADR 0010 decision
// 7b). The configuration columns are written by the template tasks acting
// for a user (InsertTx, SetProvisioned, Delete); observed_release and
// observed_used_bytes are what the build observed.
type TemplateStore struct {
	db *sql.DB
}

// NewTemplateStore returns the template store for the agent database.
func NewTemplateStore(db *sql.DB) *TemplateStore { return &TemplateStore{db: db} }

// InsertTx stores a new, unprovisioned template in the transaction that
// creates its build task. A name another template has is
// template_name_taken.
func (s *TemplateStore) InsertTx(ctx context.Context, tx *sql.Tx, nodeID string, t models.Template) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM container_templates WHERE name = ?`, t.Name).Scan(&n); err != nil {
		return fmt.Errorf("database: check template name: %w", err)
	}
	if n > 0 {
		return &models.Error{Code: models.CodeTemplateNameTaken, Message: "another template already has this name"}
	}
	pool, err := poolID(ctx, tx, nodeID, t.Pool, PoolTypeZFS)
	if err != nil {
		return err
	}
	pkgs := t.Packages
	if pkgs == nil {
		pkgs = []string{}
	}
	raw, err := json.Marshal(pkgs)
	if err != nil {
		return err
	}
	var archive any
	if t.Source == models.TemplateSourceDist {
		// The checksum is only known once the build has checked it; the
		// schema requires one for a dist template, so a placeholder of
		// zeros stands until SetProvisioned records the real one.
		archive = zeroSHA256
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO container_templates (id, node_id, storage_pool_id, name, source, packages, archive_sha256, provisioned, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		t.ID, nodeID, pool, t.Name, t.Source, string(raw), archive, formatTime(t.CreatedAt), formatTime(t.CreatedAt)); err != nil {
		return fmt.Errorf("database: insert template: %w", err)
	}
	return nil
}

// zeroSHA256 stands for the archive checksum of a dist template whose
// build has not checked it yet.
const zeroSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

const templateColumns = `t.id, t.name, p.name, t.source, t.packages, t.archive_sha256, t.provisioned,
	COALESCE(t.observed_release, ''), COALESCE(t.observed_used_bytes, 0), t.created_at`

const templateFrom = ` FROM container_templates t JOIN storage_pools p ON p.id = t.storage_pool_id`

func scanTemplate(row rowScanner) (models.Template, error) {
	var (
		t           models.Template
		pkgs        string
		archive     sql.NullString
		provisioned int
		created     string
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Pool, &t.Source, &pkgs, &archive, &provisioned,
		&t.Release, &t.UsedBytes, &created); err != nil {
		return models.Template{}, err
	}
	if err := json.Unmarshal([]byte(pkgs), &t.Packages); err != nil {
		return models.Template{}, fmt.Errorf("database: template packages: %w", err)
	}
	if archive.Valid && archive.String != zeroSHA256 {
		t.ArchiveSHA256 = archive.String
	}
	t.Provisioned = provisioned == 1
	var err error
	t.CreatedAt, err = parseTime(created)
	return t, err
}

// Get returns one template, or models.ErrNotFound.
func (s *TemplateStore) Get(ctx context.Context, id string) (models.Template, error) {
	t, err := scanTemplate(s.db.QueryRowContext(ctx, `SELECT `+templateColumns+templateFrom+` WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Template{}, fmt.Errorf("template %q: %w", id, models.ErrNotFound)
	}
	if err != nil {
		return models.Template{}, fmt.Errorf("database: get template: %w", err)
	}
	return t, nil
}

// List returns a page of templates ordered by name, and the cursor of the
// next page.
func (s *TemplateStore) List(ctx context.Context, limit int, cursor string) ([]models.Template, string, error) {
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) == 0 {
			return nil, "", models.Invalid("invalid cursor")
		}
		after = string(b)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+templateColumns+templateFrom+
		` WHERE t.name > ? ORDER BY t.name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("database: list templates: %w", err)
	}
	list := []models.Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("database: list templates: %w", err)
		}
		list = append(list, t)
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

// SetProvisioned records a finished build: the release and size it
// observed, and for a dist template the checked archive checksum.
func (s *TemplateStore) SetProvisioned(ctx context.Context, id, release, archiveSHA256 string, used int64, at time.Time) error {
	var archive any
	if archiveSHA256 != "" {
		archive = archiveSHA256
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE container_templates SET provisioned = 1, observed_release = ?, observed_used_bytes = ?, observed_at = ?,
			archive_sha256 = COALESCE(?, archive_sha256), updated_at = ?
		WHERE id = ?`, release, used, formatTime(at), archive, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: provision template: %w", err)
	}
	return oneRow(res, "template", id)
}

// Delete removes a template's row. A template a container still uses is
// refused by the schema (ON DELETE RESTRICT).
func (s *TemplateStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM container_templates WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("database: delete template: %w", err)
	}
	return oneRow(res, "template", id)
}

func oneRow(res sql.Result, what, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %q: %w", what, id, models.ErrNotFound)
	}
	return nil
}
