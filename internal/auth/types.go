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
	ID           string
	Username     string
	DisplayName  string
	PasswordHash string
	Role         Role
	Disabled     bool
	// CredentialEpoch is raised by a password change, a role change and
	// a change of the disabled flag (step 8c-1, D6).
	CredentialEpoch   uint64
	LastLoginAt       *time.Time
	PasswordChangedAt *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// IsAdmin reports whether the user has the built-in admin role.
func (u User) IsAdmin() bool { return u.Role.ID == RoleAdmin }

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
	// ForwardedSession is the identity of a request another node's
	// skep-api forwarded (step 8e-1, D26): no token, never degraded, the
	// user and role of this node's copy.
	ForwardedSession SessionKind = "forwarded"
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
	// CodeNoQuorum: a user write that cannot be committed, a login
	// without quorum that is not an administrator's (step 8c-1, D17).
	CodeNoQuorum = "no_quorum"
	// CodeUsersImportPending: a user write on a host whose 0.1 users are
	// not yet the cluster's (D14).
	CodeUsersImportPending = "users_import_pending"
	// CodeTooManyUsers: the cluster holds the most users it may (D7).
	CodeTooManyUsers = "too_many_users"
)

// Outcomes of a user write that was not committed (details.outcome of
// no_quorum).
const (
	OutcomeNotWritten       = "not_written"
	OutcomeUnknown          = "unknown"
	OutcomeWrittenNotSynced = "written_not_synced"
)

// NoQuorum returns the no_quorum error with its outcome.
func NoQuorum(outcome, message string) *models.Error {
	return &models.Error{Code: CodeNoQuorum, Message: message, Details: map[string]any{"outcome": outcome}}
}

var (
	// ErrUsernameTaken is returned when a username already exists.
	ErrUsernameTaken = &models.Error{Code: CodeUsernameTaken, Message: "a user with this username already exists"}
	// ErrLastAdmin protects the only enabled administrator from being
	// disabled, deleted, or demoted.
	ErrLastAdmin = &models.Error{Code: CodeLastAdmin, Message: "this is the only enabled administrator; it cannot be disabled, deleted, or given another role"}
	// ErrUnknownRole is returned for a role that does not exist.
	ErrUnknownRole = &models.Error{Code: CodeUnknownRole, Message: "no such role"}
)

// UserStore reads the read copy of the cluster's users, and the roles,
// from api.db (internal/apidb). A missing user or role is
// models.ErrNotFound. Nothing in it changes a user: user writes go through
// the cluster (UserWriter, step 8c-1).
type UserStore interface {
	UserByName(ctx context.Context, username string) (User, error)
	UserByID(ctx context.Context, id string) (User, error)
	// ListUsers returns every user, ordered by username.
	ListUsers(ctx context.Context) ([]User, error)
	// RecordLogin stores the time of a login (a local column).
	RecordLogin(ctx context.Context, id string, at time.Time) error
	ListRoles(ctx context.Context) ([]Role, error)
	// AdminExists reports whether an enabled administrator exists.
	AdminExists(ctx context.Context) (bool, error)
}

// User entry types a UserWriter sends (the cluster's, step 8c-1).
const (
	WriteCreate   = "user.create"
	WriteUpdate   = "user.update"
	WritePassword = "user.password"
	WriteRehash   = "user.rehash"
	WriteDelete   = "user.delete"
)

// UserWrite is one user entry: its type, who asked for it, and its data
// (without the time, which the cluster's leader writes).
type UserWrite struct {
	Type  string
	Actor Actor
	Data  any
	// Timeout bounds the whole write; 0 is the writer's default.
	Timeout time.Duration
}

// UserWriter sends user entries through the cluster. Write returns once the
// local read copy reflects the entry, or an error: ErrUsernameTaken,
// ErrLastAdmin, ErrUnknownRole, models.ErrNotFound, models.Invalid, a
// no_quorum or users_import_pending error.
type UserWriter interface {
	Write(ctx context.Context, w UserWrite) error
	// Quorum reports whether user writes can be committed now.
	Quorum() bool
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
