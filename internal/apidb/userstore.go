package apidb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/pkg/models"
)

// UserStore reads users and roles from api.db. It implements
// auth.UserStore. The users table is the read copy of the cluster's users
// (step 8c-1, D13): nothing here writes a replicated column; the copy's
// apply function (usercopy.go) is the only writer.
type UserStore struct {
	db *sql.DB
}

// NewUserStore returns the user store for the API database.
func NewUserStore(db *sql.DB) *UserStore { return &UserStore{db: db} }

const userColumns = `
	u.id, u.username, u.display_name, u.password_hash, u.disabled, u.credential_epoch,
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
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &disabled, &u.CredentialEpoch,
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
func (s *UserStore) ListUsers(ctx context.Context) ([]auth.User, error) { return listUsers(ctx, s.db) }

func listUsers(ctx context.Context, q queryer) ([]auth.User, error) {
	rows, err := q.QueryContext(ctx, `SELECT`+userColumns+` ORDER BY u.username`)
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

func enabledAdmins(ctx context.Context, q queryer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role_id = ? AND disabled = 0`, auth.RoleAdmin).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("database: count administrators: %w", err)
	}
	return n, nil
}

// AdminExists reports whether an enabled administrator exists in the copy.
func (s *UserStore) AdminExists(ctx context.Context) (bool, error) {
	n, err := enabledAdmins(ctx, s.db)
	return n > 0, err
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
