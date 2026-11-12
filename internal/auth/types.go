package auth

import (
	"context"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Built-in role IDs, created by migration api/0002.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Role is a named set of permissions. Every user has exactly one, with
// global scope.
type Role struct {
	ID          string
	Name        string
	Description string
	Permissions []string
	BuiltIn     bool
}

// User is an account. PasswordHash never leaves skep-api: the API layer
// converts a User to its public form without it.
type User struct {
	ID                string
	Username          string
	DisplayName       string
	PasswordHash      string
	Role              Role
	Disabled          bool
	LastLoginAt       *time.Time
	PasswordChangedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsAdmin reports whether the user has the built-in admin role.
func (u User) IsAdmin() bool { return u.Role.ID == RoleAdmin }

// NewUserRecord is what the store needs to create a user.
type NewUserRecord struct {
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	RoleID       string
	At           time.Time
}

// UserUpdate changes a user. Nil fields are left as they are.
type UserUpdate struct {
	DisplayName *string
	RoleID      *string
	Disabled    *bool
}

// SessionKind says how a session's token is presented.
type SessionKind string

// Session kinds. A token is accepted only through the channel it was
// issued for.
const (
	// CookieSession tokens travel in the session cookie and are never
	// shown to JavaScript.
	CookieSession SessionKind = "cookie"
	// BearerSession tokens are returned once, in the login response, and
	// sent in the Authorization header.
	BearerSession SessionKind = "bearer"
)

// EndedSession describes a session that was just deleted, for the audit
// log. It holds neither the token nor its hash.
type EndedSession struct {
	Kind     SessionKind
	SourceIP string
}

// Error codes of this package. The API maps them to statuses.
const (
	CodeUsernameTaken = "username_taken"
	CodeLastAdmin     = "last_admin"
	CodeUnknownRole   = "unknown_role"
)

var (
	// ErrUsernameTaken is returned when a username already exists.
	ErrUsernameTaken = &models.Error{Code: CodeUsernameTaken, Message: "a user with this username already exists"}
	// ErrLastAdmin protects the only enabled administrator from being
	// disabled, deleted, or demoted.
	ErrLastAdmin = &models.Error{Code: CodeLastAdmin, Message: "this is the only enabled administrator; it cannot be disabled, deleted, or given another role"}
	// ErrUnknownRole is returned for a role that does not exist.
	ErrUnknownRole = &models.Error{Code: CodeUnknownRole, Message: "no such role"}
)

// UserStore persists users, roles, and (for revocation) their sessions.
// The SQLite implementation is in internal/database. A missing user or
// role is models.ErrNotFound.
type UserStore interface {
	// CreateUser returns ErrUsernameTaken or ErrUnknownRole when it
	// applies.
	CreateUser(ctx context.Context, u NewUserRecord) (User, error)
	UserByName(ctx context.Context, username string) (User, error)
	UserByID(ctx context.Context, id string) (User, error)
	// ListUsers returns every user, ordered by username.
	ListUsers(ctx context.Context) ([]User, error)
	// UpdateUser applies upd and deletes the user's sessions in the same
	// transaction when the role or the disabled flag changed. It returns
	// ErrLastAdmin instead of disabling or demoting the only enabled
	// administrator.
	UpdateUser(ctx context.Context, id string, upd UserUpdate, at time.Time) (user User, changed UserChange, ended []EndedSession, err error)
	// SetPassword stores a new hash and deletes the user's sessions in
	// the same transaction.
	SetPassword(ctx context.Context, id, hash string, at time.Time) ([]EndedSession, error)
	// UpgradeHash replaces the hash of an unchanged password with one
	// made with stronger parameters. Sessions are kept. It does nothing
	// when the stored hash is no longer old.
	UpgradeHash(ctx context.Context, id, old, hash string) error
	// DeleteUser removes the user and its sessions. It returns
	// ErrLastAdmin for the only enabled administrator.
	DeleteUser(ctx context.Context, id string) (User, []EndedSession, error)
	RecordLogin(ctx context.Context, id string, at time.Time) error
	ListRoles(ctx context.Context) ([]Role, error)
	// AdminExists reports whether an enabled administrator exists.
	AdminExists(ctx context.Context) (bool, error)
}

// UserChange says what an update changed, for the audit log and for
// session revocation.
type UserChange struct {
	DisplayName bool
	Role        bool
	Disabled    bool
}

// Any reports whether anything changed.
func (c UserChange) Any() bool { return c.DisplayName || c.Role || c.Disabled }

// AuditWriter appends to the access audit log.
type AuditWriter interface {
	Write(ctx context.Context, e audit.AccessEntry) error
}

// Actor is who performs an action, for the audit log: a user of the API,
// or "local:<uid>" for a command run on the host.
type Actor struct {
	// ID is the user's ID. Empty for a local command.
	ID   string
	Name string
	// SourceIP and RequestID are set for API requests.
	SourceIP  string
	RequestID string
}

// Causes of a session revocation, recorded in the audit log.
const (
	CausePasswordChange = "password change"
	CauseUserDisabled   = "user disabled"
	CauseRoleChange     = "role change"
	CauseUserDeleted    = "user deleted"
	CauseAdminAction    = "administrator action"
	CauseLogout         = "logout"
)
