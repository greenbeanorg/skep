package apidb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/database"
)

// Import states of the copy (step 8c-1, D14).
const (
	ImportNotNeeded  = "not_needed"
	ImportPending    = "pending"
	ImportDone       = "done"
	ImportSuperseded = "superseded"
)

// CauseClusterReplaced is the cause of the sessions a copy of another
// cluster ends.
const CauseClusterReplaced = "the cluster's users replaced this node's"

// CopyState is the one row of cluster_copy.
type CopyState struct {
	ClusterID  string
	UsersIndex uint64
	Import     string
	ImportOpID string
}

// CopyState reads the copy's row.
func (s *UserStore) CopyState(ctx context.Context) (CopyState, error) {
	return copyState(ctx, s.db)
}

func copyState(ctx context.Context, q queryer) (CopyState, error) {
	var c CopyState
	err := q.QueryRowContext(ctx, `SELECT cluster_id, users_index, import_state, import_op_id FROM cluster_copy WHERE id = 1`).
		Scan(&c.ClusterID, &c.UsersIndex, &c.Import, &c.ImportOpID)
	if err != nil {
		return CopyState{}, fmt.Errorf("database: read the copy's state: %w", err)
	}
	return c, nil
}

// RecordImportOp stores the import's operation ID while the import is
// pending and has none yet, and returns the one stored (D14: recorded
// before it is sent, reused by a rerun).
func (s *UserStore) RecordImportOp(ctx context.Context, opID string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE cluster_copy SET import_op_id = ? WHERE id = 1 AND import_state = ? AND import_op_id = ''`,
		opID, ImportPending); err != nil {
		return "", fmt.Errorf("database: record the import's operation ID: %w", err)
	}
	c, err := s.CopyState(ctx)
	return c.ImportOpID, err
}

// FinishImportDone moves a pending import to done, in one statement with
// the cluster ID and users index the copy is at (F1 of the 8c-1 review):
// usersIndex is the import entry's index minus 1, so that the same-cluster
// rule of ApplyAnswer drops every answer read before the import, and the
// next answer (the import's) applies.
func (s *UserStore) FinishImportDone(ctx context.Context, clusterID string, usersIndex uint64) error {
	if clusterID == "" {
		return fmt.Errorf("database: an import is done only with its cluster ID")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE cluster_copy SET import_state = ?, cluster_id = ?, users_index = ? WHERE id = 1 AND import_state = ?`,
		ImportDone, clusterID, int64(usersIndex), ImportPending); err != nil {
		return fmt.Errorf("database: record the import's end: %w", err)
	}
	return nil
}

// FinishImport moves a pending import to done or superseded.
func (s *UserStore) FinishImport(ctx context.Context, state string) error {
	if state != ImportDone && state != ImportSuperseded {
		return fmt.Errorf("database: %q is not a finished import state", state)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE cluster_copy SET import_state = ? WHERE id = 1 AND import_state = ?`, state, ImportPending); err != nil {
		return fmt.Errorf("database: record the import's end: %w", err)
	}
	return nil
}

// LocalImportUsers returns the copy's rows as an import (with the host's
// own times), ordered by username.
func (s *UserStore) LocalImportUsers(ctx context.Context) ([]clusterstate.ImportUser, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]clusterstate.ImportUser, 0, len(users))
	for _, u := range users {
		iu := clusterstate.ImportUser{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, PasswordHash: u.PasswordHash,
			RoleID: u.Role.ID, Disabled: u.Disabled, CreatedAt: u.CreatedAt.Unix(), UpdatedAt: u.UpdatedAt.Unix()}
		if u.PasswordChangedAt != nil {
			iu.PasswordChangedAt = u.PasswordChangedAt.Unix()
		}
		out = append(out, iu)
	}
	return out, nil
}

// Applied says what ApplyAnswer did.
type Applied struct {
	// Changed is false when the answer was not applied (an import
	// pending, a lower or equal users index).
	Changed bool
	// Replaced is true when the copy took another cluster's users.
	Replaced bool
	// UsersIndex is the copy's users index afterwards.
	UsersIndex uint64
	// SessionsEnded counts the sessions deleted.
	SessionsEnded int
}

type endedSession struct {
	userID, username, kind, ip, cause string
}

// ApplyAnswer applies one users-read answer to the copy, in one
// transaction (step 8c-1, D13, D15). It is the only code that writes the
// replicated columns of users. The caller has checked that the answer
// comes from a node with quorum (D17).
//
//   - An import pending: nothing is written.
//   - No stored cluster ID, import done or not needed: the answer's cluster
//     is adopted and its users applied; sessions end only by the epoch rule.
//   - No stored cluster ID and the import superseded, or another stored
//     cluster ID: the users are replaced and every session is deleted.
//   - The same cluster ID: never back to a lower users index.
//
// A user whose epoch changed, or who is gone, loses every session, each
// one audited as session.revoke by the actor "system" with the cause.
func (s *UserStore) ApplyAnswer(ctx context.Context, a userapi.UsersAnswer, now time.Time) (Applied, error) {
	var out Applied
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("database: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cs, err := copyState(ctx, tx)
	if err != nil {
		return out, err
	}
	out.UsersIndex = cs.UsersIndex
	if cs.Import == ImportPending || a.ClusterID == "" {
		return out, nil
	}
	replace := (cs.ClusterID == "" && cs.Import == ImportSuperseded) || (cs.ClusterID != "" && cs.ClusterID != a.ClusterID)
	if cs.ClusterID == a.ClusterID && a.UsersIndex <= cs.UsersIndex {
		return out, nil // never back, and nothing new
	}
	var ended []endedSession
	if replace {
		if ended, err = endSessions(ctx, tx, "", CauseClusterReplaced); err != nil {
			return out, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM users`); err != nil {
			return out, fmt.Errorf("database: replace the users: %w", err)
		}
	}
	cur := map[string]auth.User{}
	if !replace {
		rows, err := listUsers(ctx, tx)
		if err != nil {
			return out, err
		}
		for _, u := range rows {
			cur[u.ID] = u
		}
	}
	want := map[string]clusterstate.User{}
	for _, u := range a.Users {
		want[u.ID] = u
	}
	// Deletes first: a new user may take a deleted one's name.
	for id, u := range cur {
		if _, ok := want[id]; ok {
			continue
		}
		e, err := endSessions(ctx, tx, id, auth.CauseUserDeleted)
		if err != nil {
			return out, err
		}
		ended = append(ended, e...)
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
			return out, fmt.Errorf("database: delete a user from the copy: %w", err)
		}
	}
	for _, u := range a.Users {
		old, exists := cur[u.ID]
		if exists && old.CredentialEpoch != u.CredentialEpoch {
			e, err := endSessions(ctx, tx, u.ID, causeOf(old, u))
			if err != nil {
				return out, err
			}
			ended = append(ended, e...)
		}
		if err := upsertUser(ctx, tx, u, exists); err != nil {
			return out, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cluster_copy SET cluster_id = ?, users_index = ? WHERE id = 1`, a.ClusterID, int64(a.UsersIndex)); err != nil {
		return out, fmt.Errorf("database: record the copy's index: %w", err)
	}
	for _, e := range ended {
		if err := insertAccess(ctx, tx, audit.AccessEntry{
			ActorName: audit.System, Action: auth.ActionSessionRevoke, ResourceType: "user", ResourceID: e.userID, Outcome: audit.Success,
			Details: map[string]any{"username": e.username, "session_kind": e.kind, "session_source_ip": e.ip, "cause": e.cause},
		}, now); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("database: commit: %w", err)
	}
	return Applied{Changed: true, Replaced: replace, UsersIndex: a.UsersIndex, SessionsEnded: len(ended)}, nil
}

// causeOf names why a user's epoch changed.
func causeOf(old auth.User, u clusterstate.User) string {
	switch {
	case old.PasswordHash != u.PasswordHash:
		return auth.CausePasswordChange
	case u.Disabled && !old.Disabled:
		return auth.CauseUserDisabled
	case old.Role.ID != u.RoleID:
		return auth.CauseRoleChange
	}
	return "credential change"
}

func unixTime(sec int64) string { return database.FormatTime(time.Unix(sec, 0).UTC()) }

func upsertUser(ctx context.Context, tx *sql.Tx, u clusterstate.User, exists bool) error {
	var pw any
	if u.PasswordChangedAt > 0 {
		pw = unixTime(u.PasswordChangedAt)
	}
	var err error
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE users SET username = ?, display_name = ?, password_hash = ?, role_id = ?, disabled = ?,
			credential_epoch = ?, password_changed_at = ?, created_at = ?, updated_at = ? WHERE id = ?`,
			u.Username, u.DisplayName, u.PasswordHash, u.RoleID, boolInt(u.Disabled), int64(u.CredentialEpoch), pw,
			unixTime(u.CreatedAt), unixTime(u.UpdatedAt), u.ID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO users (id, username, display_name, password_hash, role_id, disabled,
			credential_epoch, password_changed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.Username, u.DisplayName, u.PasswordHash, u.RoleID, boolInt(u.Disabled), int64(u.CredentialEpoch), pw,
			unixTime(u.CreatedAt), unixTime(u.UpdatedAt))
	}
	if err != nil {
		// The error text of the driver may quote values: only the
		// user's ID and name are named here, never its hash.
		return fmt.Errorf("database: write user %s (%s) to the copy", u.ID, u.Username)
	}
	return nil
}

// endSessions deletes the sessions of one user, or of every user when
// userID is "", and returns them for the audit.
func endSessions(ctx context.Context, tx *sql.Tx, userID, cause string) ([]endedSession, error) {
	q := `SELECT s.token_hash, s.user_id, u.username, s.kind, COALESCE(s.source_ip, '') FROM sessions s JOIN users u ON u.id = s.user_id`
	var args []any
	if userID != "" {
		q += ` WHERE s.user_id = ?`
		args = append(args, userID)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("database: read sessions: %w", err)
	}
	var out []endedSession
	var hashes []string
	for rows.Next() {
		var e endedSession
		var h string
		if err := rows.Scan(&h, &e.userID, &e.username, &e.kind, &e.ip); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database: read sessions: %w", err)
		}
		e.cause = cause
		out, hashes = append(out, e), append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: read sessions: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, h); err != nil {
			return nil, fmt.Errorf("database: delete a session: %w", err)
		}
	}
	return out, nil
}

// ClearImportOp drops the recorded operation ID of a pending import that
// was refused (it applied nothing).
func (s *UserStore) ClearImportOp(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE cluster_copy SET import_op_id = '' WHERE id = 1 AND import_state = ?`, ImportPending); err != nil {
		return fmt.Errorf("database: drop the import's operation ID: %w", err)
	}
	return nil
}
