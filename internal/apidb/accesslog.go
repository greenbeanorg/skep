// Package apidb is the storage of skep-api's own database, api.db: users,
// roles, sessions, and the access audit log. It implements the store
// interfaces of internal/auth.
//
// It is a separate package from internal/database, which opens and
// migrates both databases and holds the agent's stores, so that skep-agent
// does not link the identity code: the agent never opens api.db
// (ADR 0004).
package apidb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
)

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// nullable stores an empty string as NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

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
		database.FormatTime(at), nullable(e.ActorID), e.ActorName, e.Action,
		nullable(e.ResourceType), nullable(e.ResourceID), string(e.Outcome),
		nullable(e.SourceIP), nullable(e.RequestID), details)
	if err != nil {
		return fmt.Errorf("audit: write access entry: %w", err)
	}
	return nil
}

// LocalAction is an access audit entry written by a command run on the
// host ("skep-api admin ...", "skep-api tls regenerate").
type LocalAction struct {
	At         time.Time
	Actor      string
	Action     string
	ResourceID string
	Details    string
}

// ActionAPIStart is the audit action written at every start of skep-api.
const ActionAPIStart = "api.start"

// LocalSinceLastStart returns the local actions recorded since skep-api
// last started. skep-api logs them at start, so a command run while it
// was down still shows up in its log.
func (a *AccessLog) LocalSinceLastStart(ctx context.Context) ([]LocalAction, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT occurred_at, actor_name, action, COALESCE(resource_id, ''), COALESCE(details, '')
		FROM audit_log
		WHERE actor_name LIKE 'local:%'
		  AND id > COALESCE((SELECT MAX(id) FROM audit_log WHERE action = ?), 0)
		ORDER BY id`, ActionAPIStart)
	if err != nil {
		return nil, fmt.Errorf("audit: read local actions: %w", err)
	}
	defer rows.Close()
	var out []LocalAction
	for rows.Next() {
		var l LocalAction
		var at string
		if err := rows.Scan(&at, &l.Actor, &l.Action, &l.ResourceID, &l.Details); err != nil {
			return nil, fmt.Errorf("audit: read local actions: %w", err)
		}
		if l.At, err = database.ParseTime(at); err != nil {
			return nil, fmt.Errorf("audit: read local actions: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: read local actions: %w", err)
	}
	return out, nil
}
