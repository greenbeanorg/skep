package apidb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/pkg/models"
)

// UserStore stores users and roles in api.db. It implements
// auth.UserStore.
type UserStore struct {
	db *sql.DB
}

// NewUserStore returns the user store for the API database.
func NewUserStore(db *sql.DB) *UserStore { return &UserStore{db: db} }

const userColumns = `
	u.id, u.username, u.display_name, u.password_hash, u.disabled,
	u.last_login_at, u.password_changed_at, u.created_at, u.updated_at,
	r.id, r.name, r.description, r.permissions, r.built_in
	FROM users u JOIN roles r ON r.id = u.role_id`

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type scanner interface{ Scan(dest ...any) error }

func scanUser(row scanner) (auth.User, error) {
	var (
		u                    auth.User
		disabled, builtIn    int
		lastLogin, pwChanged sql.NullString
		created, updated     string
		permissions          string
	)
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &disabled,
		&lastLogin, &pwChanged, &created, &updated,
		&u.Role.ID, &u.Role.Name, &u.Role.Description, &permissions, &builtIn)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.User{}, fmt.Errorf("user: %w", models.ErrNotFound)
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("database: read user: %w", err)
	}
	u.Disabled, u.Role.BuiltIn = disabled == 1, builtIn == 1
	if err := json.Unmarshal([]byte(permissions), &u.Role.Permissions); err != nil {
		return auth.User{}, fmt.Errorf("database: role %s has invalid permissions: %w", u.Role.ID, err)
	}
	if u.CreatedAt, err = database.ParseTime(created); err != nil {
		return auth.User{}, fmt.Errorf("database: user %s: %w", u.ID, err)
	}
	if u.UpdatedAt, err = database.ParseTime(updated); err != nil {
		return auth.User{}, fmt.Errorf("database: user %s: %w", u.ID, err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{lastLogin, &u.LastLoginAt}, {pwChanged, &u.PasswordChangedAt}} {
		if f.src.Valid {
			t, err := database.ParseTime(f.src.String)
			if err != nil {
				return auth.User{}, fmt.Errorf("database: user %s: %w", u.ID, err)
			}
			*f.dst = &t
		}
	}
	return u, nil
}

func userByID(ctx context.Context, q queryer, id string) (auth.User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT`+userColumns+` WHERE u.id = ?`, id))
}

// UserByID returns a user, or models.ErrNotFound.
func (s *UserStore) UserByID(ctx context.Context, id string) (auth.User, error) {
	return userByID(ctx, s.db, id)
}

// UserByName returns a user by username, or models.ErrNotFound.
func (s *UserStore) UserByName(ctx context.Context, username string) (auth.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT`+userColumns+` WHERE u.username = ?`, username))
}

// ListUsers returns every user, ordered by username.
func (s *UserStore) ListUsers(ctx context.Context) ([]auth.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT`+userColumns+` ORDER BY u.username`)
	if err != nil {
		return nil, fmt.Errorf("database: list users: %w", err)
	}
	defer rows.Close()
	out := []auth.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: list users: %w", err)
	}
	return out, nil
}

func roleExists(ctx context.Context, q queryer, id string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("database: read role: %w", err)
	}
	return n == 1, nil
}

// CreateUser stores a new user.
func (s *UserStore) CreateUser(ctx context.Context, u auth.NewUserRecord) (auth.User, error) {
	var created auth.User
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if ok, err := roleExists(ctx, tx, u.RoleID); err != nil {
			return err
		} else if !ok {
			return auth.ErrUnknownRole
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username = ?`, u.Username).Scan(&taken); err != nil {
			return fmt.Errorf("database: create user: %w", err)
		}
		if taken > 0 {
			return auth.ErrUsernameTaken
		}
		at := database.FormatTime(u.At)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, username, display_name, password_hash, role_id,
				password_changed_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.Username, u.DisplayName, u.PasswordHash, u.RoleID, at, at, at); err != nil {
			return fmt.Errorf("database: create user: %w", err)
		}
		var err error
		created, err = userByID(ctx, tx, u.ID)
		return err
	})
	return created, err
}

// inTx runs fn in a transaction. Transactions take the write lock when
// they begin (see database.Open), so a read followed by a write cannot be overtaken.
func (s *UserStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

func enabledAdmins(ctx context.Context, q queryer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role_id = ? AND disabled = 0`, auth.RoleAdmin).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("database: count administrators: %w", err)
	}
	return n, nil
}

// AdminExists reports whether an enabled administrator exists.
func (s *UserStore) AdminExists(ctx context.Context) (bool, error) {
	n, err := enabledAdmins(ctx, s.db)
	return n > 0, err
}

// deleteSessions removes every session of a user and returns what they
// were, for the audit log.
func deleteSessions(ctx context.Context, tx *sql.Tx, userID string) ([]auth.EndedSession, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM sessions WHERE user_id = ? RETURNING kind, COALESCE(source_ip, '')`, userID)
	if err != nil {
		return nil, fmt.Errorf("database: delete sessions: %w", err)
	}
	defer rows.Close()
	var out []auth.EndedSession
	for rows.Next() {
		var e auth.EndedSession
		var kind string
		if err := rows.Scan(&kind, &e.SourceIP); err != nil {
			return nil, fmt.Errorf("database: delete sessions: %w", err)
		}
		e.Kind = auth.SessionKind(kind)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: delete sessions: %w", err)
	}
	return out, nil
}

// UpdateUser applies upd. See auth.UserStore.
func (s *UserStore) UpdateUser(ctx context.Context, id string, upd auth.UserUpdate, at time.Time) (auth.User, auth.UserChange, []auth.EndedSession, error) {
	var (
		user    auth.User
		changed auth.UserChange
		ended   []auth.EndedSession
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := userByID(ctx, tx, id)
		if err != nil {
			return err
		}
		var sets []string
		var args []any
		if upd.DisplayName != nil && *upd.DisplayName != cur.DisplayName {
			changed.DisplayName = true
			sets, args = append(sets, "display_name = ?"), append(args, *upd.DisplayName)
		}
		if upd.RoleID != nil && *upd.RoleID != cur.Role.ID {
			if ok, err := roleExists(ctx, tx, *upd.RoleID); err != nil {
				return err
			} else if !ok {
				return auth.ErrUnknownRole
			}
			changed.Role = true
			sets, args = append(sets, "role_id = ?"), append(args, *upd.RoleID)
		}
		if upd.Disabled != nil && *upd.Disabled != cur.Disabled {
			changed.Disabled = true
			sets, args = append(sets, "disabled = ?"), append(args, boolInt(*upd.Disabled))
		}
		if !changed.Any() {
			user = cur
			return nil
		}
		// The only enabled administrator must stay one.
		losesAdmin := (changed.Role && cur.IsAdmin()) || (changed.Disabled && *upd.Disabled && cur.IsAdmin())
		if losesAdmin && !cur.Disabled {
			n, err := enabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return auth.ErrLastAdmin
			}
		}
		sets, args = append(sets, "updated_at = ?"), append(args, database.FormatTime(at), id)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			return fmt.Errorf("database: update user: %w", err)
		}
		if changed.Role || changed.Disabled {
			if ended, err = deleteSessions(ctx, tx, id); err != nil {
				return err
			}
		}
		user, err = userByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return auth.User{}, auth.UserChange{}, nil, err
	}
	return user, changed, ended, nil
}

// SetPassword stores a new hash and ends the user's sessions.
func (s *UserStore) SetPassword(ctx context.Context, id, hash string, at time.Time) ([]auth.EndedSession, error) {
	var ended []auth.EndedSession
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?`,
			hash, database.FormatTime(at), database.FormatTime(at), id)
		if err != nil {
			return fmt.Errorf("database: set password: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("user: %w", models.ErrNotFound)
		}
		ended, err = deleteSessions(ctx, tx, id)
		return err
	})
	return ended, err
}

// UpgradeHash replaces old with hash, if old is still what is stored.
func (s *UserStore) UpgradeHash(ctx context.Context, id, old, hash string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?`, hash, id, old); err != nil {
		return fmt.Errorf("database: upgrade password hash: %w", err)
	}
	return nil
}

// DeleteUser removes a user and, through the foreign key, its sessions.
func (s *UserStore) DeleteUser(ctx context.Context, id string) (auth.User, []auth.EndedSession, error) {
	var (
		user  auth.User
		ended []auth.EndedSession
	)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if user, err = userByID(ctx, tx, id); err != nil {
			return err
		}
		if user.IsAdmin() && !user.Disabled {
			n, err := enabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return auth.ErrLastAdmin
			}
		}
		if ended, err = deleteSessions(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
			return fmt.Errorf("database: delete user: %w", err)
		}
		return nil
	})
	if err != nil {
		return auth.User{}, nil, err
	}
	return user, ended, nil
}

// RecordLogin stores the time of a successful login.
func (s *UserStore) RecordLogin(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, database.FormatTime(at), id); err != nil {
		return fmt.Errorf("database: record login: %w", err)
	}
	return nil
}

// ListRoles returns every role, built-in roles first.
func (s *UserStore) ListRoles(ctx context.Context) ([]auth.Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, description, permissions, built_in FROM roles ORDER BY built_in DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("database: list roles: %w", err)
	}
	defer rows.Close()
	out := []auth.Role{}
	for rows.Next() {
		var r auth.Role
		var permissions string
		var builtIn int
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &permissions, &builtIn); err != nil {
			return nil, fmt.Errorf("database: list roles: %w", err)
		}
		if err := json.Unmarshal([]byte(permissions), &r.Permissions); err != nil {
			return nil, fmt.Errorf("database: role %s has invalid permissions: %w", r.ID, err)
		}
		r.BuiltIn = builtIn == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database: list roles: %w", err)
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
