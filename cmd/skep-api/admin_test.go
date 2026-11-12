package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/termpass"
)

const testPassword = "correct horse battery staple"

// fastConfig writes a configuration with the cheapest allowed Argon2id
// parameters, so the tests do not spend their time hashing.
func fastConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(path, []byte(`{"auth": {"argon2": {"memory_kib": 19456, "passes": 2}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// adminEnv is a test environment whose terminal answers the password
// prompts with the given lines, in order.
func adminEnv(stdin string, typed ...string) (env, *bytes.Buffer, *[]string) {
	var out bytes.Buffer
	prompts := &[]string{}
	e := testEnv(&out, io.Discard)
	e.uid = 4242
	e.stdin = strings.NewReader(stdin)
	e.readPassword = func(_ context.Context, prompt string) (string, error) {
		*prompts = append(*prompts, prompt)
		if len(typed) == 0 {
			return "", termpass.ErrNoTerminal
		}
		line := typed[0]
		typed = typed[1:]
		return line, nil
	}
	return e, &out, prompts
}

func openAPIDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "api", "api.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type userRow struct {
	ID, Hash, Role string
	Disabled       int
}

func readUser(t *testing.T, db *sql.DB, username string) (userRow, bool) {
	t.Helper()
	var u userRow
	err := db.QueryRow(`SELECT id, password_hash, role_id, disabled FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Hash, &u.Role, &u.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return userRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return u, true
}

func verifies(t *testing.T, password, hash string) bool {
	t.Helper()
	h, err := auth.NewHasher(auth.MinParams, 1, auth.DefaultHashWait)
	if err != nil {
		t.Fatal(err)
	}
	ok, _, err := h.Verify(context.Background(), password, hash)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAdminCreateOnTheTerminal(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	e, out, prompts := adminEnv("", testPassword, testPassword)
	args := []string{"admin", "create", "--username", "andy", "--display-name", "Andy", "-dev", "-config", cfg, "-data-dir", dataDir}
	if err := run(context.Background(), args, e); err != nil {
		t.Fatal(err)
	}
	if len(*prompts) != 2 || !strings.Contains((*prompts)[0], "andy") {
		t.Errorf("prompts = %q, want the password asked for twice", *prompts)
	}
	if !strings.Contains(out.String(), `Administrator "andy" created`) || strings.Contains(out.String(), testPassword) {
		t.Errorf("output = %q", out.String())
	}

	db := openAPIDB(t, dataDir)
	u, ok := readUser(t, db, "andy")
	if !ok || u.Role != "admin" || u.Disabled != 0 {
		t.Fatalf("user = %+v, %v", u, ok)
	}
	if !strings.HasPrefix(u.Hash, "$argon2id$v=19$m=19456,t=2,p=1$") || !verifies(t, testPassword, u.Hash) {
		t.Errorf("stored hash = %q", u.Hash)
	}
	// Audited with the invoking user as the actor.
	var actor, details string
	if err := db.QueryRow(`SELECT actor_name, details FROM audit_log WHERE action = 'user.create'`).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "local:4242" || !strings.Contains(details, `"username":"andy"`) || strings.Contains(details, "argon2") {
		t.Errorf("audit: actor %q, details %s", actor, details)
	}

	// A second administrator can be created at any time; the same name
	// cannot.
	e, _, _ = adminEnv("", testPassword, testPassword)
	args[3] = "carol"
	if err := run(context.Background(), args, e); err != nil {
		t.Errorf("second administrator: %v", err)
	}
	e, _, prompts = adminEnv("", testPassword, testPassword)
	err := run(context.Background(), args, e)
	if err == nil || !strings.Contains(err.Error(), "already exists") || len(*prompts) != 0 {
		t.Errorf("duplicate: %v (prompts %q)", err, *prompts)
	}
}

func TestAdminCreateRefusals(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	for name, tt := range map[string]struct {
		args  []string
		stdin string
		typed []string
		want  string
	}{
		"passwords differ":        {[]string{"--username", "andy"}, "", []string{testPassword, testPassword + "x"}, "not the same"},
		"short password":          {[]string{"--username", "andy"}, "", []string{"short", "short"}, "12 to 256"},
		"no terminal":             {[]string{"--username", "andy"}, "", nil, "--password-stdin"},
		"empty stdin":             {[]string{"--username", "andy", "--password-stdin"}, "", nil, "no password"},
		"short stdin password":    {[]string{"--username", "andy", "--password-stdin"}, "short\n", nil, "12 to 256"},
		"no username":             {[]string{"--password-stdin"}, testPassword + "\n", nil, "username"},
		"bad username":            {[]string{"--username", "Root Admin", "--password-stdin"}, testPassword + "\n", nil, "username"},
		"password as an argument": {[]string{"--username", "andy", "--password", testPassword}, "", nil, "flag provided but not defined"},
		"stray argument":          {[]string{"--username", "andy", "--password-stdin", testPassword}, "", nil, "unexpected argument"},
	} {
		e, _, prompts := adminEnv(tt.stdin, tt.typed...)
		err := run(context.Background(), append(append([]string{"admin", "create"}, tt.args...), base...), e)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tt.want)
		}
		if name == "short password" && len(*prompts) != 1 {
			t.Errorf("%s: asked %d times; a password that breaks the rules is refused before the repeat", name, len(*prompts))
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "api", "api.db")); err == nil {
		if _, ok := readUser(t, openAPIDB(t, dataDir), "andy"); ok {
			t.Error("a refused command created the user")
		}
	}
	// The environment is not a way in either.
	t.Setenv("SKEP_PASSWORD", testPassword)
	t.Setenv("SKEP_ADMIN_PASSWORD", testPassword)
	e, _, _ := adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", "andy"}, base...), e); err == nil {
		t.Error("a password was taken from the environment")
	}
	for name, args := range map[string][]string{"no command": {"admin"}, "unknown command": {"admin", "destroy"}} {
		if err := run(context.Background(), args, e); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}

func TestAdminCreateFromStdin(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	e, _, prompts := adminEnv(testPassword + "\r\nignored second line\n")
	err := run(context.Background(), []string{"admin", "create", "--username", "andy", "--password-stdin", "-dev", "-config", cfg, "-data-dir", dataDir}, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(*prompts) != 0 {
		t.Errorf("--password-stdin still prompted: %q", *prompts)
	}
	u, ok := readUser(t, openAPIDB(t, dataDir), "andy")
	if !ok || !verifies(t, testPassword, u.Hash) {
		t.Error("the password from standard input was not stored correctly (one line, without its line ending)")
	}
}

func TestAdminResetPassword(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	e, _, _ := adminEnv(testPassword + "\n")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", "andy", "--password-stdin"}, base...), e); err != nil {
		t.Fatal(err)
	}
	before, _ := readUser(t, openAPIDB(t, dataDir), "andy")

	// The user has a session.
	rw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := auth.NewToken()
	if _, err := rw.Exec(`INSERT INTO sessions (token_hash, user_id, kind, created_at, last_seen_at, expires_at, source_ip)
		VALUES (?, ?, 'cookie', '2026-11-12T12:00:00.000Z', '2026-11-12T12:00:00.000Z', '2026-11-13T00:00:00.000Z', '192.0.2.9')`,
		auth.HashToken(token), before.ID); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	const newPassword = "an entirely different passphrase"
	e, out, _ := adminEnv("", newPassword, newPassword)
	if err := run(context.Background(), append([]string{"admin", "reset-password", "--username", "andy"}, base...), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sessions were ended") {
		t.Errorf("output = %q", out.String())
	}
	db := openAPIDB(t, dataDir)
	after, _ := readUser(t, db, "andy")
	if after.Hash == before.Hash || !verifies(t, newPassword, after.Hash) || verifies(t, testPassword, after.Hash) {
		t.Error("the password was not replaced")
	}
	var sessions int
	db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	if sessions != 0 {
		t.Error("the user's session survived the password reset")
	}
	var set, revoked int
	db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'user.password.set' AND actor_name = 'local:4242'`).Scan(&set)
	db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'session.revoke' AND actor_name = 'local:4242' AND details LIKE '%password change%'`).Scan(&revoked)
	if set != 1 || revoked != 1 {
		t.Errorf("audit: %d password entries, %d revocations", set, revoked)
	}

	e, _, _ = adminEnv(testPassword + "\n")
	err = run(context.Background(), append([]string{"admin", "reset-password", "--username", "nobody", "--password-stdin"}, base...), e)
	if err == nil || !strings.Contains(err.Error(), "no user") {
		t.Errorf("unknown user: %v", err)
	}
}

func TestAdminHashBenchmark(t *testing.T) {
	e, out, _ := adminEnv("")
	if err := run(context.Background(), []string{"admin", "hash-benchmark", "--rounds", "2", "-dev", "-config", fastConfig(t), "-data-dir", t.TempDir()}, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "argon2id, 19 MiB, 2 passes, 1 lane") || !strings.Contains(out.String(), "mean ") {
		t.Errorf("output = %q", out.String())
	}
	if err := run(context.Background(), []string{"admin", "hash-benchmark", "--rounds", "0", "-dev", "-data-dir", t.TempDir()}, e); err == nil {
		t.Error("--rounds 0 accepted")
	}
}

// TestLocalCommandsAreLoggedAtTheNextStart: an administrator created while
// skep-api was not running shows up in its log when it starts, once.
func TestLocalCommandsAreLoggedAtTheNextStart(t *testing.T) {
	dir, err := os.MkdirTemp("", "skep-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dataDir, cfg := filepath.Join(dir, "data"), fastConfig(t)
	e, _, _ := adminEnv(testPassword + "\n")
	if err := run(context.Background(), []string{"admin", "create", "--username", "andy", "--password-stdin", "-dev", "-config", cfg, "-data-dir", dataDir}, e); err != nil {
		t.Fatal(err)
	}
	serverArgs := []string{"-dev", "-config", cfg, "-data-dir", dataDir, "-agent-socket", filepath.Join(dir, "absent.sock")}

	_, logs := startAPIWithLogs(t, serverArgs...)
	if !strings.Contains(logs.String(), `"msg":"local command since the last start"`) ||
		!strings.Contains(logs.String(), `"actor":"local:4242"`) || !strings.Contains(logs.String(), `"action":"user.create"`) {
		t.Errorf("the first start did not log the local command:\n%s", logs.String())
	}
	logs.stop()

	_, logs = startAPIWithLogs(t, serverArgs...)
	if strings.Contains(logs.String(), "local command since the last start") {
		t.Errorf("the second start logged the local command again:\n%s", logs.String())
	}
}

// TestConfigAndAuthAgreeOnArgon2Bounds: internal/config repeats the bounds
// so that the agent does not link the password hashing code.
func TestConfigAndAuthAgreeOnArgon2Bounds(t *testing.T) {
	if err := run(context.Background(), []string{"admin", "hash-benchmark", "--rounds", "1", "-dev", "-data-dir", t.TempDir(), "-config", fastConfig(t)}, testEnv(io.Discard, io.Discard)); err != nil {
		t.Errorf("the configuration's minimum is refused by internal/auth: %v", err)
	}
	if auth.DefaultParams.MemoryKiB != config.DefaultArgon2MemoryKiB || auth.DefaultParams.Passes != config.DefaultArgon2Passes ||
		auth.MinParams.MemoryKiB != config.MinArgon2MemoryKiB || auth.MinParams.Passes != config.MinArgon2Passes {
		t.Errorf("auth defaults %+v, minimum %+v differ from internal/config", auth.DefaultParams, auth.MinParams)
	}
	// The configuration's maximum is accepted too.
	top := auth.Params{MemoryKiB: config.MaxArgon2MemoryKiB, Passes: config.MaxArgon2Passes, Lanes: 1}
	if err := top.Validate(); err != nil {
		t.Errorf("the configuration's maximum is refused by internal/auth: %v", err)
	}
}

// createLocal creates a user with a local command and returns its ID.
func createLocal(t *testing.T, base []string, username string) {
	t.Helper()
	e, _, _ := adminEnv(testPassword + "\n")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", username, "--password-stdin"}, base...), e); err != nil {
		t.Fatal(err)
	}
}

// addSession gives a user a session row, as a login would.
func addSession(t *testing.T, dataDir, userID string) {
	t.Helper()
	rw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	token, _ := auth.NewToken()
	if _, err := rw.Exec(`INSERT INTO sessions (token_hash, user_id, kind, created_at, last_seen_at, expires_at, source_ip)
		VALUES (?, ?, 'bearer', '2026-11-12T12:00:00.000Z', '2026-11-12T12:00:00.000Z', '2099-11-13T00:00:00.000Z', '192.0.2.9')`,
		auth.HashToken(token), userID); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdminDisable(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	createLocal(t, base, "andy")

	// The only enabled administrator is refused, and the refusal is
	// audited.
	e, _, _ := adminEnv("")
	err := run(context.Background(), append([]string{"admin", "disable", "--username", "andy"}, base...), e)
	if err == nil || !strings.Contains(err.Error(), "only enabled administrator") {
		t.Fatalf("disabling the last administrator: %v", err)
	}
	db := openAPIDB(t, dataDir)
	if u, _ := readUser(t, db, "andy"); u.Disabled != 0 {
		t.Fatal("the last administrator was disabled")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'user.update' AND outcome = 'denied' AND actor_name = 'local:4242'`); n != 1 {
		t.Errorf("%d audited refusals, want 1", n)
	}

	// With a second administrator, it works and ends the sessions.
	createLocal(t, base, "carol")
	andy, _ := readUser(t, db, "andy")
	addSession(t, dataDir, andy.ID)
	e, out, _ := adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "disable", "--username", "andy"}, base...), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"andy" was disabled`) {
		t.Errorf("output = %q", out.String())
	}
	if u, _ := readUser(t, db, "andy"); u.Disabled != 1 {
		t.Error("andy is not disabled")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, andy.ID); n != 0 {
		t.Error("the disabled user's session survived")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'session.revoke' AND actor_name = 'local:4242' AND details LIKE '%user disabled%'`); n != 1 {
		t.Errorf("%d audited revocations, want 1", n)
	}

	// Again: nothing to do. carol is now the only enabled administrator.
	e, out, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "disable", "--username", "andy"}, base...), e); err != nil || !strings.Contains(out.String(), "already disabled") {
		t.Errorf("second disable: %v, %q", err, out.String())
	}
	e, _, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "disable", "--username", "carol"}, base...), e); err == nil {
		t.Error("the remaining enabled administrator was disabled")
	}
	for name, args := range map[string][]string{
		"unknown user": {"--username", "nobody"},
		"no username":  {},
		"bad username": {"--username", "Not Valid"},
	} {
		e, _, _ = adminEnv("")
		if err := run(context.Background(), append(append([]string{"admin", "disable"}, args...), base...), e); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}

func TestAdminDelete(t *testing.T) {
	dataDir, cfg := t.TempDir(), fastConfig(t)
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	createLocal(t, base, "andy")
	db := openAPIDB(t, dataDir)

	e, _, _ := adminEnv("")
	err := run(context.Background(), append([]string{"admin", "delete", "--username", "andy", "--yes"}, base...), e)
	if err == nil || !strings.Contains(err.Error(), "only enabled administrator") {
		t.Fatalf("deleting the last administrator: %v", err)
	}
	if _, ok := readUser(t, db, "andy"); !ok {
		t.Fatal("the last administrator was deleted")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'user.delete' AND outcome = 'denied'`); n != 1 {
		t.Errorf("%d audited refusals, want 1", n)
	}

	createLocal(t, base, "carol")
	andy, _ := readUser(t, db, "andy")
	addSession(t, dataDir, andy.ID)

	// Without --yes nothing happens.
	e, _, _ = adminEnv("")
	err = run(context.Background(), append([]string{"admin", "delete", "--username", "andy"}, base...), e)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("without --yes: %v", err)
	}
	if _, ok := readUser(t, db, "andy"); !ok {
		t.Fatal("deleted without --yes")
	}

	e, out, _ := adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "delete", "--username", "andy", "--yes"}, base...), e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"andy" was deleted`) {
		t.Errorf("output = %q", out.String())
	}
	if _, ok := readUser(t, db, "andy"); ok {
		t.Error("andy still exists")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sessions`); n != 0 {
		t.Error("the deleted user's session survived")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'user.delete' AND outcome = 'success' AND actor_name = 'local:4242'`); n != 1 {
		t.Errorf("%d audited deletions, want 1", n)
	}
	e, _, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "delete", "--username", "andy", "--yes"}, base...), e); err == nil || !strings.Contains(err.Error(), "no user") {
		t.Errorf("deleting again: %v", err)
	}

	// A disabled administrator is not the one the rule protects.
	createLocal(t, base, "dave")
	e, _, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "disable", "--username", "dave"}, base...), e); err != nil {
		t.Fatal(err)
	}
	e, _, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "delete", "--username", "dave", "--yes"}, base...), e); err != nil {
		t.Errorf("deleting a disabled administrator: %v", err)
	}
}
