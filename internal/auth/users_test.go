package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/greenbeanorg/skep/internal/apidb"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// auditLog collects access audit entries.
type auditLog struct {
	mu      sync.Mutex
	entries []audit.AccessEntry
}

func (a *auditLog) Write(_ context.Context, e audit.AccessEntry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

// find returns the entries with the given action.
func (a *auditLog) find(action string) []audit.AccessEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.AccessEntry
	for _, e := range a.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (a *auditLog) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = nil
}

// fixture is a real api.db with the services on top.
type fixture struct {
	db     *sql.DB
	store  *apidb.UserStore
	hasher *auth.Hasher
	audit  *auditLog
	users  *auth.Users
	authn  *auth.Authenticator
	clock  *clock
}

// clock is a settable time source for session lifetimes and limits.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	hasher, err := auth.NewHasher(auth.MinParams, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, store: apidb.NewUserStore(db), hasher: hasher, audit: &auditLog{},
		clock: &clock{t: time.Now().UTC().Truncate(time.Millisecond)}}
	f.users = auth.NewUsers(f.store, hasher, f.audit, quiet())
	f.authn, err = auth.NewAuthenticator(f.authnOptions())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) authnOptions() auth.AuthenticatorOptions {
	return auth.AuthenticatorOptions{
		Users: f.store, Sessions: apidb.NewSessionStore(f.db), Hasher: f.hasher, Audit: f.audit,
		Throttle: auth.DefaultThrottleConfig(),
		Session:  auth.SessionConfig{IdleTimeout: auth.DefaultIdleTimeout, AbsoluteTimeout: auth.DefaultAbsoluteTimeout},
		Logger:   quiet(), Now: f.clock.now,
	}
}

var (
	local = auth.Actor{Name: "local:0"}
	admin = auth.Actor{ID: "admin-id", Name: "root-admin", SourceIP: "192.0.2.1", RequestID: "req-1"}
)

const goodPassword = "correct horse battery staple"

func (f *fixture) create(t *testing.T, username, role string) auth.User {
	t.Helper()
	u, err := f.users.Create(context.Background(), local, auth.NewUser{Username: username, Password: goodPassword, RoleID: role})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// addSession inserts a session row for a user.
func (f *fixture) addSession(t *testing.T, userID string, kind auth.SessionKind, ip string) {
	t.Helper()
	token, _ := auth.NewToken()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := f.db.Exec(`INSERT INTO sessions (token_hash, user_id, kind, created_at, last_seen_at, expires_at, source_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, auth.HashToken(token), userID, string(kind), now, now, now, ip); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) sessions(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if ok, err := f.users.AdminExists(ctx); err != nil || ok {
		t.Fatalf("a fresh database has an administrator: %v, %v", ok, err)
	}
	u, err := f.users.Create(ctx, local, auth.NewUser{Username: "andy", DisplayName: "Andy", Password: goodPassword, RoleID: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == "" || u.Username != "andy" || u.DisplayName != "Andy" || !u.IsAdmin() || u.Disabled ||
		u.PasswordChangedAt == nil || u.LastLoginAt != nil {
		t.Errorf("user = %+v", u)
	}
	if len(u.Role.Permissions) != 1 || u.Role.Permissions[0] != "*" || !u.Role.BuiltIn {
		t.Errorf("admin role = %+v", u.Role)
	}
	// What is stored is an Argon2id hash of the password, with the
	// configured parameters; never the password.
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(u.PasswordHash, goodPassword) {
		t.Errorf("stored hash = %q", u.PasswordHash)
	}
	if ok, _, err := f.hasher.Verify(ctx, goodPassword, u.PasswordHash); !ok || err != nil {
		t.Errorf("the stored hash does not verify: %v", err)
	}
	if ok, _ := f.users.AdminExists(ctx); !ok {
		t.Error("AdminExists is false after creating an administrator")
	}

	entries := f.audit.find(auth.ActionUserCreate)
	if len(entries) != 1 || entries[0].ActorName != "local:0" || entries[0].ActorID != "" ||
		entries[0].ResourceID != u.ID || entries[0].Outcome != audit.Success ||
		entries[0].Details["username"] != "andy" || entries[0].Details["role"] != "admin" {
		t.Errorf("audit = %+v", entries)
	}
	for _, e := range f.audit.entries {
		for _, v := range e.Details {
			if s, ok := v.(string); ok && (strings.Contains(s, goodPassword) || strings.Contains(s, "$argon2id$")) {
				t.Errorf("the audit log contains the password or its hash: %+v", e)
			}
		}
	}

	// The default role is the least privileged one.
	v, err := f.users.Create(ctx, admin, auth.NewUser{Username: "viewer1", Password: goodPassword})
	if err != nil || v.Role.ID != auth.RoleViewer {
		t.Errorf("default role: %+v, %v", v.Role, err)
	}
}

func TestCreateUserErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)

	var me *models.Error
	for name, tt := range map[string]struct {
		in   auth.NewUser
		code string
	}{
		"duplicate":      {auth.NewUser{Username: "andy", Password: goodPassword}, auth.CodeUsernameTaken},
		"bad username":   {auth.NewUser{Username: "Andy", Password: goodPassword}, models.CodeInvalidRequest},
		"short password": {auth.NewUser{Username: "bob", Password: "short"}, models.CodeInvalidRequest},
		"unknown role":   {auth.NewUser{Username: "bob", Password: goodPassword, RoleID: "superuser"}, auth.CodeUnknownRole},
		"bad display":    {auth.NewUser{Username: "bob", Password: goodPassword, DisplayName: "a\nb"}, models.CodeInvalidRequest},
	} {
		_, err := f.users.Create(ctx, admin, tt.in)
		if !errors.As(err, &me) || me.Code != tt.code {
			t.Errorf("%s: error = %v, want code %s", name, err, tt.code)
		}
	}
	if users, _ := f.users.List(ctx); len(users) != 1 {
		t.Errorf("a refused creation left a user behind: %d users", len(users))
	}
}

func TestSetPasswordEndsSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	other := f.create(t, "bob", auth.RoleViewer)
	f.addSession(t, u.ID, auth.CookieSession, "192.0.2.10")
	f.addSession(t, u.ID, auth.BearerSession, "192.0.2.11")
	f.addSession(t, other.ID, auth.CookieSession, "192.0.2.12")
	f.audit.reset()

	if err := f.users.SetPassword(ctx, local, u.ID, "a new and different passphrase"); err != nil {
		t.Fatal(err)
	}
	after, _ := f.users.ByID(ctx, u.ID)
	if ok, _, _ := f.hasher.Verify(ctx, "a new and different passphrase", after.PasswordHash); !ok {
		t.Error("the new password does not verify")
	}
	if ok, _, _ := f.hasher.Verify(ctx, goodPassword, after.PasswordHash); ok {
		t.Error("the old password still verifies")
	}
	if f.sessions(t, u.ID) != 0 {
		t.Error("the user's sessions survived a password change")
	}
	if f.sessions(t, other.ID) != 1 {
		t.Error("another user's session was ended")
	}
	if set := f.audit.find(auth.ActionPasswordSet); len(set) != 1 || set[0].ActorName != "local:0" || set[0].ResourceID != u.ID {
		t.Errorf("password audit = %+v", set)
	}
	// One revocation entry per session, with its kind, its address, and
	// the cause.
	revoked := f.audit.find(auth.ActionSessionRevoke)
	if len(revoked) != 2 {
		t.Fatalf("revocations audited = %+v", revoked)
	}
	kinds := map[any]any{}
	for _, e := range revoked {
		kinds[e.Details["session_kind"]] = e.Details["session_source_ip"]
		if e.Details["cause"] != auth.CausePasswordChange || e.ResourceID != u.ID || e.Details["username"] != "andy" {
			t.Errorf("revocation entry = %+v", e)
		}
	}
	if kinds["cookie"] != "192.0.2.10" || kinds["bearer"] != "192.0.2.11" {
		t.Errorf("revoked sessions = %v", kinds)
	}

	if err := f.users.SetPassword(ctx, local, u.ID, "short"); err == nil {
		t.Error("a short password was accepted")
	}
	if err := f.users.SetPassword(ctx, local, "no-such-user", goodPassword); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestUpdateUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)
	u := f.create(t, "bob", auth.RoleViewer)
	f.addSession(t, u.ID, auth.CookieSession, "192.0.2.10")
	f.audit.reset()

	// A display name change keeps the sessions.
	name := "Bob B."
	got, err := f.users.Update(ctx, admin, u.ID, auth.UserUpdate{DisplayName: &name})
	if err != nil || got.DisplayName != name || f.sessions(t, u.ID) != 1 {
		t.Fatalf("display name: %+v, %v, sessions %d", got, err, f.sessions(t, u.ID))
	}
	// A role change ends them, and is effective at once.
	role := auth.RoleOperator
	got, err = f.users.Update(ctx, admin, u.ID, auth.UserUpdate{RoleID: &role})
	if err != nil || got.Role.ID != auth.RoleOperator || f.sessions(t, u.ID) != 0 {
		t.Fatalf("role: %+v, %v, sessions %d", got.Role, err, f.sessions(t, u.ID))
	}
	if r := f.audit.find(auth.ActionSessionRevoke); len(r) != 1 || r[0].Details["cause"] != auth.CauseRoleChange || r[0].ActorID != "admin-id" {
		t.Errorf("revocation = %+v", r)
	}
	// Disabling ends them too.
	f.addSession(t, u.ID, auth.BearerSession, "192.0.2.11")
	f.audit.reset()
	yes := true
	got, err = f.users.Update(ctx, admin, u.ID, auth.UserUpdate{Disabled: &yes})
	if err != nil || !got.Disabled || f.sessions(t, u.ID) != 0 {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	if r := f.audit.find(auth.ActionSessionRevoke); len(r) != 1 || r[0].Details["cause"] != auth.CauseUserDisabled {
		t.Errorf("revocation = %+v", r)
	}
	if upd := f.audit.find(auth.ActionUserUpdate); len(upd) != 1 || upd[0].Details["disabled"] != true || upd[0].SourceIP != "192.0.2.1" {
		t.Errorf("update audit = %+v", upd)
	}

	// Nothing to change: no audit entry.
	f.audit.reset()
	if _, err := f.users.Update(ctx, admin, u.ID, auth.UserUpdate{Disabled: &yes, DisplayName: &name}); err != nil || len(f.audit.entries) != 0 {
		t.Errorf("no-op update: %v, audit %+v", err, f.audit.entries)
	}
	unknown := "superuser"
	if _, err := f.users.Update(ctx, admin, u.ID, auth.UserUpdate{RoleID: &unknown}); !errors.Is(err, auth.ErrUnknownRole) {
		t.Errorf("unknown role: %v", err)
	}
	if _, err := f.users.Update(ctx, admin, "nope", auth.UserUpdate{Disabled: &yes}); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestLastAdministratorIsProtected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	only := f.create(t, "andy", auth.RoleAdmin)
	f.create(t, "bob", auth.RoleOperator)
	f.addSession(t, only.ID, auth.CookieSession, "192.0.2.10")
	f.audit.reset()

	yes, viewer := true, auth.RoleViewer
	if _, err := f.users.Update(ctx, admin, only.ID, auth.UserUpdate{Disabled: &yes}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("disable the only admin: %v", err)
	}
	if _, err := f.users.Update(ctx, admin, only.ID, auth.UserUpdate{RoleID: &viewer}); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("demote the only admin: %v", err)
	}
	if err := f.users.Delete(ctx, admin, only.ID); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("delete the only admin: %v", err)
	}
	still, _ := f.users.ByID(ctx, only.ID)
	if !still.IsAdmin() || still.Disabled || f.sessions(t, only.ID) != 1 {
		t.Errorf("a refused change altered the administrator: %+v", still)
	}
	// Every refusal is audited as a denial.
	if d := append(f.audit.find(auth.ActionUserUpdate), f.audit.find(auth.ActionUserDelete)...); len(d) != 3 {
		t.Errorf("denials audited = %+v", d)
	} else {
		for _, e := range d {
			if e.Outcome != audit.Denied || e.Details["reason"] != auth.CodeLastAdmin {
				t.Errorf("denial entry = %+v", e)
			}
		}
	}

	// A disabled second administrator does not count.
	second := f.create(t, "carol", auth.RoleAdmin)
	if _, err := f.users.Update(ctx, admin, second.ID, auth.UserUpdate{Disabled: &yes}); err != nil {
		t.Fatalf("disabling one of two admins: %v", err)
	}
	if err := f.users.Delete(ctx, admin, only.ID); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("delete the only enabled admin: %v", err)
	}
	// With a second enabled administrator the first may go.
	no := false
	if _, err := f.users.Update(ctx, admin, second.ID, auth.UserUpdate{Disabled: &no}); err != nil {
		t.Fatal(err)
	}
	f.audit.reset()
	if err := f.users.Delete(ctx, admin, only.ID); err != nil {
		t.Fatalf("delete one of two enabled admins: %v", err)
	}
	if _, err := f.users.ByID(ctx, only.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("deleted user still exists: %v", err)
	}
	if f.sessions(t, only.ID) != 0 {
		t.Error("the deleted user's session survived")
	}
	if r := f.audit.find(auth.ActionSessionRevoke); len(r) != 1 || r[0].Details["cause"] != auth.CauseUserDeleted {
		t.Errorf("revocation = %+v", r)
	}
	if err := f.users.Delete(ctx, admin, "nope"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("delete unknown user: %v", err)
	}
}

func TestRolesAndLookups(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	roles, err := f.users.Roles(ctx)
	if err != nil || len(roles) != 3 {
		t.Fatalf("roles = %+v, %v", roles, err)
	}
	perms := map[string][]string{}
	for _, r := range roles {
		if !r.BuiltIn || r.ID != r.Name || r.Description == "" {
			t.Errorf("role = %+v", r)
		}
		perms[r.ID] = r.Permissions
	}
	has := func(role, perm string) bool {
		for _, p := range perms[role] {
			if p == perm {
				return true
			}
		}
		return false
	}
	// ADR 0006 decision 5.
	if !has("admin", "*") {
		t.Error("admin is not stored as *")
	}
	for _, p := range []string{"system:read", "node:read", "task:read", "task:cancel", "user:read", "role:read", "audit:read"} {
		if !has("operator", p) {
			t.Errorf("operator lacks %s", p)
		}
	}
	if has("operator", "user:manage") || has("operator", "*") {
		t.Error("operator can manage users")
	}
	for _, p := range []string{"system:read", "node:read", "task:read", "role:read"} {
		if !has("viewer", p) {
			t.Errorf("viewer lacks %s", p)
		}
	}
	for _, p := range []string{"audit:read", "user:read", "user:manage", "task:cancel", "*"} {
		if has("viewer", p) {
			t.Errorf("viewer has %s", p)
		}
	}
	// Built-in roles cannot be edited or deleted, even with direct SQL.
	if _, err := f.db.Exec(`UPDATE roles SET permissions = '["*"]' WHERE id = 'viewer'`); err == nil {
		t.Error("a built-in role could be changed")
	}
	if _, err := f.db.Exec(`DELETE FROM roles WHERE id = 'viewer'`); err == nil {
		t.Error("a built-in role could be deleted")
	}

	f.create(t, "zoe", auth.RoleViewer)
	f.create(t, "andy", auth.RoleAdmin)
	users, err := f.users.List(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "andy" || users[1].Username != "zoe" {
		t.Errorf("list = %+v, %v", users, err)
	}
	if _, err := f.users.ByName(ctx, "nobody"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown name: %v", err)
	}
	// Input that is not a username never reaches the database.
	if _, err := f.users.ByName(ctx, "' OR 1=1 --"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("malformed name: %v", err)
	}
}

// The audit log's actor for the agent's own actions cannot be a user's name.
func TestCreateUserRefusesTheSystemName(t *testing.T) {
	if auth.ReservedUsername != audit.System {
		t.Fatalf("reserved %q, audit actor %q", auth.ReservedUsername, audit.System)
	}
	f := newFixture(t)
	for _, name := range []string{"system", "System", "SYSTEM"} {
		_, err := f.users.Create(context.Background(), admin, auth.NewUser{Username: name, Password: goodPassword})
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if users, _ := f.users.List(context.Background()); len(users) != 0 {
		t.Errorf("a refused creation left %d users", len(users))
	}
}
