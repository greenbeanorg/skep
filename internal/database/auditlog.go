package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
)

// timeLayout is how timestamps are stored: UTC, millisecond precision, the
// same shape the schema's strftime defaults produce. It sorts as text.
const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AuditLog appends entries to the agent's operation audit log. It
// implements audit.Writer.
type AuditLog struct {
	db  *sql.DB
	now func() time.Time
}

// NewAuditLog returns the audit writer for the agent database.
func NewAuditLog(db *sql.DB) *AuditLog {
	return &AuditLog{db: db, now: time.Now}
}

// Write validates and appends one entry.
func (a *AuditLog) Write(ctx context.Context, e audit.Entry) error {
	return insertAudit(ctx, a.db, e, a.now())
}

// insertAudit appends an entry using db or an open transaction, so task
// state changes can write their audit entries atomically.
func insertAudit(ctx context.Context, x execer, e audit.Entry, at time.Time) error {
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
		INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action,
			resource_type, resource_id, task_id, phase, outcome, task_status,
			source_ip, request_id, details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(at), e.ActorID, e.ActorName, int64(e.PeerUID), e.Action,
		nullable(e.ResourceType), nullable(e.ResourceID), nullable(e.TaskID),
		string(e.Phase), string(e.Outcome), nullable(e.TaskStatus),
		nullable(e.SourceIP), nullable(e.RequestID), details)
	if err != nil {
		return fmt.Errorf("audit: write entry: %w", err)
	}
	return nil
}

// nullable stores an empty string as NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
