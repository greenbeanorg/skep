package apidb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/pkg/models"
)

// SessionStore stores sessions in api.db. It implements auth.SessionStore.
// Only the SHA-256 of a session token is ever stored.
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore returns the session store for the API database.
func NewSessionStore(db *sql.DB) *SessionStore { return &SessionStore{db: db} }

// CreateSession stores a new session.
func (s *SessionStore) CreateSession(ctx context.Context, sess auth.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, kind, created_at, last_seen_at, expires_at, source_ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.UserID, string(sess.Kind),
		database.FormatTime(sess.CreatedAt), database.FormatTime(sess.LastSeenAt), database.FormatTime(sess.ExpiresAt),
		nullable(sess.SourceIP), nullable(sess.UserAgent))
	if err != nil {
		return fmt.Errorf("database: create session: %w", err)
	}
	return nil
}

// SessionByHash returns a session, or models.ErrNotFound.
func (s *SessionStore) SessionByHash(ctx context.Context, tokenHash string) (auth.Session, error) {
	var (
		sess                       auth.Session
		kind                       string
		created, lastSeen, expires string
		ip, ua                     sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, kind, created_at, last_seen_at, expires_at, source_ip, user_agent
		FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&sess.TokenHash, &sess.UserID, &kind, &created, &lastSeen, &expires, &ip, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Session{}, fmt.Errorf("session: %w", models.ErrNotFound)
	}
	if err != nil {
		return auth.Session{}, fmt.Errorf("database: read session: %w", err)
	}
	sess.Kind, sess.SourceIP, sess.UserAgent = auth.SessionKind(kind), ip.String, ua.String
	for _, f := range []struct {
		src string
		dst *time.Time
	}{{created, &sess.CreatedAt}, {lastSeen, &sess.LastSeenAt}, {expires, &sess.ExpiresAt}} {
		t, err := database.ParseTime(f.src)
		if err != nil {
			return auth.Session{}, fmt.Errorf("database: read session: %w", err)
		}
		*f.dst = t
	}
	return sess, nil
}

// TouchSession records that a session was used.
func (s *SessionStore) TouchSession(ctx context.Context, tokenHash string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
		database.FormatTime(at), tokenHash); err != nil {
		return fmt.Errorf("database: touch session: %w", err)
	}
	return nil
}

// DeleteSession removes one session and reports whether it existed.
func (s *SessionStore) DeleteSession(ctx context.Context, tokenHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return false, fmt.Errorf("database: delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteExpiredSessions removes sessions past their absolute end or not
// used since idleBefore.
func (s *SessionStore) DeleteExpiredSessions(ctx context.Context, now, idleBefore time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		database.FormatTime(now), database.FormatTime(idleBefore))
	if err != nil {
		return 0, fmt.Errorf("database: delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
