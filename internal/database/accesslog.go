package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
)

// AccessLog appends entries to skep-api's access audit log in api.db. The
// table is append-only: the schema rejects updates and deletes.
type AccessLog struct {
	db  *sql.DB
	now func() time.Time
}

// NewAccessLog returns the audit writer for the API database.
func NewAccessLog(db *sql.DB) *AccessLog {
	return &AccessLog{db: db, now: time.Now}
}

// Write validates and appends one entry.
func (a *AccessLog) Write(ctx context.Context, e audit.AccessEntry) error {
	return insertAccess(ctx, a.db, e, a.now())
}

// insertAccess appends an entry using db or an open transaction.
func insertAccess(ctx context.Context, x execer, e audit.AccessEntry, at time.Time) error {
	if err := e.Validate(); err != nil {
		return err
	}
	var details any
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: encode details: %w", err)
		}
		details = string(b)
	}
	_, err := x.ExecContext(ctx, `
		INSERT INTO audit_log (occurred_at, actor_id, actor_name, action, resource_type,
			resource_id, outcome, source_ip, request_id, details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(at), nullable(e.ActorID), e.ActorName, e.Action,
		nullable(e.ResourceType), nullable(e.ResourceID), string(e.Outcome),
		nullable(e.SourceIP), nullable(e.RequestID), details)
	if err != nil {
		return fmt.Errorf("audit: write access entry: %w", err)
	}
	return nil
}
