package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Audit actions written by this package.
const (
	ActionUserCreate    = "user.create"
	ActionUserUpdate    = "user.update"
	ActionUserDelete    = "user.delete"
	ActionPasswordSet   = "user.password.set"
	ActionSessionRevoke = "session.revoke"
)

// Users manages accounts. Every change is written to the access audit log,
// and every change that affects what a user may do ends that user's
// sessions.
type Users struct {
	store  UserStore
	hasher *Hasher
	audit  AuditWriter
	log    *slog.Logger
	now    func() time.Time
}

// NewUsers returns the user service.
func NewUsers(store UserStore, hasher *Hasher, auditLog AuditWriter, log *slog.Logger) *Users {
	if log == nil {
		log = slog.Default()
	}
	return &Users{store: store, hasher: hasher, audit: auditLog, log: log, now: time.Now}
}

// NewUser describes an account to create.
type NewUser struct {
	Username    string
	DisplayName string
	Password    string
	// RoleID defaults to the viewer role.
	RoleID string
}

// record writes an audit entry. An entry that cannot be written is logged
// with its content instead of being lost.
func record(ctx context.Context, w AuditWriter, log *slog.Logger, actor Actor, e audit.AccessEntry) {
	e.ActorID, e.ActorName, e.SourceIP, e.RequestID = actor.ID, actor.Name, actor.SourceIP, actor.RequestID
	if err := w.Write(ctx, e); err != nil {
		log.Error("could not write to the access audit log", "error", err,
			"action", e.Action, "actor", e.ActorName, "outcome", string(e.Outcome),
			"resource_type", e.ResourceType, "resource_id", e.ResourceID)
	}
}

// recordRevocations writes one audit entry per ended session, with its
// kind, the address it was created from, and the cause.
func recordRevocations(ctx context.Context, w AuditWriter, log *slog.Logger, actor Actor, user User, ended []EndedSession, cause string) {
	for _, s := range ended {
		record(ctx, w, log, actor, audit.AccessEntry{
			Action: ActionSessionRevoke, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success,
			Details: map[string]any{
				"username": user.Username, "session_kind": string(s.Kind),
				"session_source_ip": s.SourceIP, "cause": cause,
			},
		})
	}
}

// Create adds a user.
func (u *Users) Create(ctx context.Context, actor Actor, in NewUser) (User, error) {
	if in.Username == ReservedUsername {
		return User{}, models.Invalid("the username \"system\" is reserved for the agent's own audit entries")
	}
	if err := ValidateUsername(in.Username); err != nil {
		return User{}, err
	}
	if err := ValidateDisplayName(in.DisplayName); err != nil {
		return User{}, err
	}
	if err := ValidatePassword(in.Password); err != nil {
		return User{}, err
	}
	if in.RoleID == "" {
		in.RoleID = RoleViewer
	}
	// Check the name before spending a hash on it. The store checks
	// again, atomically.
	if _, err := u.store.UserByName(ctx, in.Username); err == nil {
		return User{}, ErrUsernameTaken
	} else if !errors.Is(err, models.ErrNotFound) {
		return User{}, err
	}
	hash, err := u.hasher.Hash(ctx, in.Password)
	if err != nil {
		return User{}, err
	}
	id, err := ids.NewUUID()
	if err != nil {
		return User{}, fmt.Errorf("auth: generate user ID: %w", err)
	}
	user, err := u.store.CreateUser(ctx, NewUserRecord{
		ID: id, Username: in.Username, DisplayName: in.DisplayName,
		PasswordHash: hash, RoleID: in.RoleID, At: u.now().UTC(),
	})
	if err != nil {
		return User{}, err
	}
	record(ctx, u.audit, u.log, actor, audit.AccessEntry{
		Action: ActionUserCreate, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success,
		Details: map[string]any{"username": user.Username, "role": user.Role.Name},
	})
	u.log.Info("user created", "user_id", user.ID, "username", user.Username, "role", user.Role.Name, "by", actor.Name)
	return user, nil
}

// SetPassword replaces a user's password and ends all of that user's
// sessions. The caller has already decided that actor may do this.
func (u *Users) SetPassword(ctx context.Context, actor Actor, userID, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	user, err := u.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	hash, err := u.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	ended, err := u.store.SetPassword(ctx, user.ID, hash, u.now().UTC())
	if err != nil {
		return err
	}
	record(ctx, u.audit, u.log, actor, audit.AccessEntry{
		Action: ActionPasswordSet, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success,
		Details: map[string]any{"username": user.Username, "sessions_ended": len(ended)},
	})
	recordRevocations(ctx, u.audit, u.log, actor, user, ended, CausePasswordChange)
	u.log.Info("password set", "user_id", user.ID, "username", user.Username, "by", actor.Name, "sessions_ended", len(ended))
	return nil
}

// ByName returns a user by username.
func (u *Users) ByName(ctx context.Context, username string) (User, error) {
	if !ValidUsername(username) {
		return User{}, models.ErrNotFound
	}
	return u.store.UserByName(ctx, username)
}

// ByID returns a user by ID.
func (u *Users) ByID(ctx context.Context, id string) (User, error) {
	return u.store.UserByID(ctx, id)
}

// List returns every user, ordered by username.
func (u *Users) List(ctx context.Context) ([]User, error) { return u.store.ListUsers(ctx) }

// Roles returns every role.
func (u *Users) Roles(ctx context.Context) ([]Role, error) { return u.store.ListRoles(ctx) }

// AdminExists reports whether an enabled administrator exists. Until one
// does, nobody can log in.
func (u *Users) AdminExists(ctx context.Context) (bool, error) { return u.store.AdminExists(ctx) }

// Update changes a user's display name, role, or disabled flag. A change of
// role or a disabling ends the user's sessions. The only enabled
// administrator cannot be disabled or demoted.
func (u *Users) Update(ctx context.Context, actor Actor, userID string, upd UserUpdate) (User, error) {
	if upd.DisplayName != nil {
		if err := ValidateDisplayName(*upd.DisplayName); err != nil {
			return User{}, err
		}
	}
	user, changed, ended, err := u.store.UpdateUser(ctx, userID, upd, u.now().UTC())
	if err != nil {
		if errors.Is(err, ErrLastAdmin) {
			record(ctx, u.audit, u.log, actor, audit.AccessEntry{
				Action: ActionUserUpdate, ResourceType: "user", ResourceID: userID, Outcome: audit.Denied,
				Details: map[string]any{"reason": CodeLastAdmin},
			})
		}
		return User{}, err
	}
	if !changed.Any() {
		return user, nil
	}
	details := map[string]any{"username": user.Username}
	if changed.DisplayName {
		details["display_name_changed"] = true
	}
	if changed.Role {
		details["role"] = user.Role.Name
	}
	if changed.Disabled {
		details["disabled"] = user.Disabled
	}
	record(ctx, u.audit, u.log, actor, audit.AccessEntry{
		Action: ActionUserUpdate, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success, Details: details,
	})
	cause := CauseRoleChange
	if changed.Disabled && user.Disabled {
		cause = CauseUserDisabled
	}
	recordRevocations(ctx, u.audit, u.log, actor, user, ended, cause)
	u.log.Info("user updated", "user_id", user.ID, "username", user.Username, "by", actor.Name,
		"role_changed", changed.Role, "disabled_changed", changed.Disabled, "sessions_ended", len(ended))
	return user, nil
}

// Delete removes a user and ends its sessions. The only enabled
// administrator cannot be deleted.
func (u *Users) Delete(ctx context.Context, actor Actor, userID string) error {
	user, ended, err := u.store.DeleteUser(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrLastAdmin) {
			record(ctx, u.audit, u.log, actor, audit.AccessEntry{
				Action: ActionUserDelete, ResourceType: "user", ResourceID: userID, Outcome: audit.Denied,
				Details: map[string]any{"reason": CodeLastAdmin},
			})
		}
		return err
	}
	record(ctx, u.audit, u.log, actor, audit.AccessEntry{
		Action: ActionUserDelete, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success,
		Details: map[string]any{"username": user.Username, "role": user.Role.Name},
	})
	recordRevocations(ctx, u.audit, u.log, actor, user, ended, CauseUserDeleted)
	u.log.Info("user deleted", "user_id", user.ID, "username", user.Username, "by", actor.Name)
	return nil
}
