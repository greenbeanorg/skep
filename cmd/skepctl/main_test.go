package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/ctlstate"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/termpass"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/client"
	"github.com/greenbeanorg/skep/pkg/models"
)

const testPassword = "correct horse battery staple"

// fakeBackend is the node side of the API: what skep-agent would answer.
type fakeBackend struct{}

func (fakeBackend) Health(context.Context) models.Health {
	return models.Health{Status: models.HealthOK}
}

func (fakeBackend) System(context.Context) (models.System, error) {
	return models.System{
		NodeID: "node-1", Hostname: "hearst", Platform: models.PlatformFreeBSD, ProductionHost: true,
		OS:           models.OSInfo{Name: "FreeBSD", Release: "15.1-RELEASE-p6"},
		Capabilities: models.CapabilitySummary{Bhyve: models.Capability{Status: models.StatusAvailable, Reason: "vmm loaded"}},
	}, nil
}

func (fakeBackend) Hardware(_ context.Context, id string) (models.Hardware, error) {
	if id != "node-1" {
		return models.Hardware{}, models.ErrNotFound
	}
	pct := 1
	return models.Hardware{
		NodeID: id, Platform: models.PlatformFreeBSD,
		CPU:    models.CPUInfo{Model: "Test CPU", Cores: 2, Threads: 2},
		Memory: models.MemoryInfo{TotalBytes: 2 << 30, AvailableBytes: 1 << 30},
		Storage: models.StorageInfo{ZFS: models.ZFSInfo{
			Capability: models.Capability{Status: models.StatusAvailable},
			Pools:      []models.ZFSPool{{Name: "skeptest", Health: "ONLINE", SizeBytes: 1 << 30, CapacityPercent: &pct}},
		}},
		Warnings: []string{"a warning with a \x1b[31mcontrol sequence and a \u202ebidi override"},
	}, nil
}

func (fakeBackend) Tasks(_ context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	if opts.Status != "" && !opts.Status.Valid() {
		return models.TaskList{}, models.Invalid("unknown status")
	}
	return models.TaskList{
		Items:      []models.Task{{ID: "task-1", Type: "test.sleep", Status: models.TaskSucceeded, Actor: models.TaskActor{Name: "andy"}}},
		NextCursor: "cursor-2",
	}, nil
}

func (fakeBackend) Task(_ context.Context, id string) (models.TaskDetail, error) {
	if id != "task-1" {
		return models.TaskDetail{}, models.ErrNotFound
	}
	return models.TaskDetail{
		Task:   models.Task{ID: "task-1", Type: "test.sleep", Status: models.TaskSucceeded},
		Events: []models.TaskEvent{{Kind: "state", Level: "info", Message: "succeeded"}},
	}, nil
}

func (fakeBackend) CancelTask(context.Context, string) (models.Task, error) {
	return models.Task{}, errors.New("not used")
}

// stack is a real skep-api identity stack and handler over TLS 1.3.
type stack struct {
	db       *sql.DB
	authn    *auth.Authenticator
	srv      *httptest.Server
	fakeVMs  *fakeVMs
	requests atomic.Int32
}

func newStack(t *testing.T) *stack { return newStackWith(t, nil) }

// newStackWith serves *cert if it is not nil, as with an operator-supplied
// certificate (the test can replace it: a renewal); otherwise httptest's
// self-signed one.
func newStackWith(t *testing.T, cert **tls.Certificate) *stack {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hasher, err := auth.NewHasher(auth.MinParams, 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	access := apidb.NewAccessLog(db)
	store := apidb.NewUserStore(db)
	users := auth.NewUsers(store, hasher, access, quiet)
	throttle := auth.DefaultThrottleConfig()
	throttle.PerAddress = 1000
	authn, err := auth.NewAuthenticator(auth.AuthenticatorOptions{
		Users: store, Sessions: apidb.NewSessionStore(db), Hasher: hasher, Audit: access, Throttle: throttle,
		Session: auth.SessionConfig{IdleTimeout: auth.DefaultIdleTimeout, AbsoluteTimeout: auth.DefaultAbsoluteTimeout},
		Logger:  quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]string{"andy": auth.RoleAdmin, "vera": auth.RoleViewer} {
		if _, err := users.Create(ctx, auth.Actor{Name: "local:0"}, auth.NewUser{Username: name, Password: testPassword, RoleID: role}); err != nil {
			t.Fatal(err)
		}
	}
	vms := &fakeVMs{}
	h := api.NewHandler(api.Options{
		Backend: fakeBackend{}, VMs: vms, Auth: authn, Users: users, Audit: access, AuditLog: access, Logger: quiet,
	})
	s := &stack{db: db, authn: authn, fakeVMs: vms}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		h.ServeHTTP(w, r)
	}))
	if cert == nil {
		s.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
		s.srv.StartTLS()
	} else {
		// Not StartTLS: its own certificate would be served to a client
		// that sends no SNI, as one connecting to an IP address does.
		s.srv.Listener = tls.NewListener(s.srv.Listener, &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return *cert, nil },
		})
		s.srv.Start()
		s.srv.URL = strings.Replace(s.srv.URL, "http://", "https://", 1)
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stack) server() string { return s.srv.URL }

func (s *stack) vms(*testing.T) *fakeVMs { return s.fakeVMs }
func (s *stack) pin() string {
	if c := s.srv.Certificate(); c != nil {
		return client.PublicKeyPin(c)
	}
	cert, err := client.FetchCertificate(context.Background(), s.server())
	if err != nil {
		panic(err)
	}
	return client.PublicKeyPin(cert)
}

func (s *stack) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// terminal answers the prompts of one skepctl run.
type terminal struct {
	passwords []string
	confirm   *bool // nil: no terminal
	prompts   []string
}

func (term *terminal) env(stateDir string, stdin string, stdout, stderr io.Writer) env {
	return env{
		stdin: strings.NewReader(stdin), stdout: stdout, stderr: stderr, stateDir: stateDir, now: time.Now,
		readPassword: func(_ context.Context, prompt string) (string, error) {
			term.prompts = append(term.prompts, prompt)
			if len(term.passwords) == 0 {
				return "", termpass.ErrNoTerminal
			}
			pw := term.passwords[0]
			term.passwords = term.passwords[1:]
			return pw, nil
		},
		confirm: func(_ context.Context, prompt string) (bool, error) {
			term.prompts = append(term.prompts, prompt)
			if term.confirm == nil {
				return false, termpass.ErrNoTerminal
			}
			return *term.confirm, nil
		},
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// skepctl runs one command and returns its stdout, stderr, and error.
func skepctl(stateDir string, term *terminal, stdin string, args ...string) (string, string, error) {
	if term == nil {
		term = &terminal{}
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), args, term.env(stateDir, stdin, &out, &errOut))
	return out.String(), errOut.String(), err
}

func login(t *testing.T, s *stack, stateDir, username string) {
	t.Helper()
	_, _, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--server", s.server(), "--username", username, "--trust-key", s.pin())
	if err != nil {
		t.Fatalf("login as %s: %v", username, err)
	}
}

func TestLoginTrustOnFirstUseAndReadCommands(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	term := &terminal{passwords: []string{testPassword}, confirm: yes()}
	out, errOut, err := skepctl(stateDir, term, "", "login", "--server", s.server(), "--username", "andy")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, s.pin()) || !strings.Contains(errOut, "skep-api tls fingerprint") {
		t.Errorf("the key was not shown for comparison:\n%s", errOut)
	}
	if len(term.prompts) != 2 || !strings.Contains(term.prompts[0], "Trust this key") || !strings.Contains(term.prompts[1], "Password for andy") {
		t.Errorf("prompts = %q", term.prompts)
	}
	if !strings.Contains(out, "Logged in to "+s.server()+" as andy (role admin)") || strings.Contains(out+errOut, testPassword) {
		t.Errorf("output = %q", out)
	}

	// The pin and the token are stored, 0600, in a 0700 directory.
	store := ctlstate.New(stateDir)
	pin, ok, err := store.Trust(s.server())
	if err != nil || !ok || pin.PublicKeySHA256 != s.pin() {
		t.Fatalf("pin = %+v, %v, %v", pin, ok, err)
	}
	sess, ok, _ := store.Session(s.server())
	if !ok || sess.Username != "andy" || sess.Token == "" {
		t.Fatalf("session = %+v", sess)
	}
	for _, name := range []string{ctlstate.PinsFile, ctlstate.SessionsFile} {
		info, err := os.Stat(filepath.Join(stateDir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, mode %v", name, err, info.Mode().Perm())
		}
	}
	// The server has a bearer session, and audited the login.
	if n := s.count(t, `SELECT COUNT(*) FROM sessions WHERE kind = 'bearer'`); n != 1 {
		t.Errorf("%d bearer sessions on the server", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'auth.login' AND outcome = 'success' AND details LIKE '%bearer%'`); n != 1 {
		t.Errorf("%d audited bearer logins", n)
	}

	// The read-only commands, without --server: the last login is used.
	out, _, err = skepctl(stateDir, nil, "", "system")
	if err != nil || !strings.Contains(out, "hearst") || !strings.Contains(out, "freebsd (production host)") || !strings.Contains(out, "vmm loaded") {
		t.Errorf("system: %v\n%s", err, out)
	}
	out, _, err = skepctl(stateDir, nil, "", "system", "--json")
	var sys models.System
	if err != nil || json.Unmarshal([]byte(out), &sys) != nil || sys.NodeID != "node-1" {
		t.Errorf("system --json: %v\n%s", err, out)
	}
	out, _, err = skepctl(stateDir, nil, "", "hardware")
	if err != nil || !strings.Contains(out, "skeptest") || !strings.Contains(out, "2.0 GiB total") {
		t.Errorf("hardware: %v\n%s", err, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Error("a control character from the server reached the terminal")
	}
	if strings.Contains(out, "\u202e") || !strings.Contains(out, "a bidi override") {
		t.Errorf("a format character from the server reached the terminal:\n%s", out)
	}
	if _, _, err = skepctl(stateDir, nil, "", "hardware", "--node", "node-9"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("hardware of an unknown node: %v", err)
	}
	out, errOut, err = skepctl(stateDir, nil, "", "tasks", "list", "--status", "succeeded", "--limit", "5")
	if err != nil || !strings.Contains(out, "task-1") || !strings.Contains(errOut, "--cursor cursor-2") {
		t.Errorf("tasks list: %v\n%s\n%s", err, out, errOut)
	}
	if _, _, err = skepctl(stateDir, nil, "", "tasks", "list", "--status", "bogus"); err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Errorf("tasks list with a bad status: %v", err)
	}
	out, _, err = skepctl(stateDir, nil, "", "tasks", "show", "task-1", "--json")
	var detail models.TaskDetail
	if err != nil || json.Unmarshal([]byte(out), &detail) != nil || len(detail.Events) != 1 {
		t.Errorf("tasks show --json: %v\n%s", err, out)
	}
	out, _, err = skepctl(stateDir, nil, "", "tasks", "show", "task-1")
	if err != nil || !strings.Contains(out, "succeeded") || !strings.Contains(out, "Events:") {
		t.Errorf("tasks show: %v\n%s", err, out)
	}
	out, _, err = skepctl(stateDir, nil, "", "audit", "list", "--limit", "50")
	if err != nil || !strings.Contains(out, "auth.login") || !strings.Contains(out, "user.create") {
		t.Errorf("audit list: %v\n%s", err, out)
	}

	// Logout ends the session on the server and forgets the token.
	out, _, err = skepctl(stateDir, nil, "", "logout")
	if err != nil || !strings.Contains(out, "Logged out") {
		t.Fatalf("logout: %v %s", err, out)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM sessions`); n != 0 {
		t.Errorf("%d sessions left on the server after logout", n)
	}
	if _, ok, _ := store.Session(s.server()); ok {
		t.Error("the token is still stored after logout")
	}
	if _, ok, _ := store.Trust(s.server()); !ok {
		t.Error("logout forgot the pinned key")
	}
	if _, _, err = skepctl(stateDir, nil, "", "system"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("system after logout: %v", err)
	}
	if _, _, err = skepctl(stateDir, nil, "", "logout"); err == nil {
		t.Error("a second logout succeeded")
	}
}

func TestFirstContactRefusals(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	base := []string{"login", "--server", s.server(), "--username", "andy"}

	// Declined: nothing is stored, and the password is never asked for.
	term := &terminal{passwords: []string{testPassword}, confirm: no()}
	if _, _, err := skepctl(stateDir, term, "", base...); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Errorf("declined: %v", err)
	}
	if len(term.prompts) != 1 {
		t.Errorf("prompts = %q, want only the question", term.prompts)
	}
	// No terminal to ask on: the error says how to pass the key.
	_, _, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "", base...)
	if err == nil || !strings.Contains(err.Error(), "--trust-key "+s.pin()) {
		t.Errorf("no terminal: %v", err)
	}
	// --trust-key with another key is refused.
	other := "sha256:" + strings.Repeat("0", 64)
	if _, _, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "", append(base, "--trust-key", other)...); err == nil || !strings.Contains(err.Error(), "nothing was trusted") {
		t.Errorf("wrong --trust-key: %v", err)
	}
	if _, ok, _ := ctlstate.New(stateDir).Trust(s.server()); ok {
		t.Fatal("a refused key was pinned")
	}
	if n := s.requests.Load(); n != 0 {
		t.Errorf("%d HTTP requests reached the server before any key was trusted", n)
	}

	// With the right --trust-key and the password on standard input.
	term = &terminal{}
	if _, _, err := skepctl(stateDir, term, testPassword+"\n", append(base, "--trust-key", s.pin(), "--password-stdin")...); err != nil {
		t.Fatal(err)
	}
	if len(term.prompts) != 0 {
		t.Errorf("prompted although everything was given: %q", term.prompts)
	}
}

func TestChangedKeyIsRefused(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "andy")
	store := ctlstate.New(stateDir)
	sess, _, _ := store.Session(s.server())

	// Pretend the server's key changed: pin another one.
	old := "sha256:" + strings.Repeat("ab", 32)
	if err := store.SetTrust(s.server(), ctlstate.Trust{PublicKeySHA256: old}); err != nil {
		t.Fatal(err)
	}
	before := s.requests.Load()
	_, _, err := skepctl(stateDir, nil, "", "system")
	if err == nil || !strings.Contains(err.Error(), "has changed") || !strings.Contains(err.Error(), "presented: "+s.pin()) {
		t.Fatalf("system with a changed key: %v", err)
	}
	if s.requests.Load() != before {
		t.Error("a request (with the token) reached a server whose key changed")
	}
	if got, ok, _ := store.Session(s.server()); !ok || got != sess {
		t.Error("the token was dropped because of a key change")
	}
	// Login without naming the new key: refused, before the password.
	term := &terminal{passwords: []string{testPassword}, confirm: yes()}
	_, _, err = skepctl(stateDir, term, "", "login", "--server", s.server(), "--username", "andy")
	if err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Errorf("login with a changed key: %v", err)
	}
	if s.requests.Load() != before {
		t.Error("the password was sent to a server whose key changed")
	}
	if len(term.prompts) != 0 {
		t.Errorf("prompted although the key had changed: %q", term.prompts)
	}
	// Naming it replaces the pin.
	_, errOut, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--server", s.server(), "--username", "andy", "--trust-key", s.pin())
	if err != nil || !strings.Contains(errOut, "old: "+old) {
		t.Fatalf("accepting the new key: %v\n%s", err, errOut)
	}
	if p, _, _ := store.Trust(s.server()); p.PublicKeySHA256 != s.pin() {
		t.Error("the pin was not replaced")
	}
	// The previous session was ended on the server, not left behind.
	if n := s.count(t, `SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions on the server, want only the new one", n)
	}
}

func TestEndedSessionAndPermissions(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "vera")

	// A viewer may read the system but not the audit log.
	if _, _, err := skepctl(stateDir, nil, "", "system"); err != nil {
		t.Errorf("viewer system: %v", err)
	}
	_, _, err := skepctl(stateDir, nil, "", "audit", "list")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("viewer audit: %v", err)
	}
	if _, ok, _ := ctlstate.New(stateDir).Session(s.server()); !ok {
		t.Error("a 403 dropped the session")
	}

	// Revoked on the server (as a password reset does): the token is
	// forgotten and the error says to log in.
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	_, _, err = skepctl(stateDir, nil, "", "system")
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Errorf("revoked session: %v", err)
	}
	if _, ok, _ := ctlstate.New(stateDir).Session(s.server()); ok {
		t.Error("the ended session's token is still stored")
	}
}

func TestLoginFailures(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	_, _, err := skepctl(stateDir, &terminal{passwords: []string{"not the password"}}, "",
		"login", "--server", s.server(), "--username", "andy", "--trust-key", s.pin())
	if err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Errorf("wrong password: %v", err)
	}
	if _, ok, _ := ctlstate.New(stateDir).Session(s.server()); ok {
		t.Error("a failed login stored a session")
	}
	if _, ok, _ := ctlstate.New(stateDir).Trust(s.server()); !ok {
		t.Error("the confirmed key was not kept after a failed password")
	}
	// No terminal for the password.
	_, _, err = skepctl(stateDir, &terminal{}, "", "login", "--server", s.server(), "--username", "andy")
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("no terminal: %v", err)
	}
}

func TestUsageErrors(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	for name, args := range map[string][]string{
		"no command":            {},
		"unknown command":       {"frobnicate"},
		"tasks without command": {"tasks"},
		"tasks unknown":         {"tasks", "delete"},
		"tasks show without ID": {"tasks", "show"},
		"tasks show two IDs":    {"tasks", "show", "a", "b"},
		"audit without command": {"audit"},
		"audit unknown":         {"audit", "clear"},
		"system stray argument": {"system", "extra"},
		"login no username":     {"login", "--server", "localhost"},
		"login bad pin":         {"login", "--server", "localhost", "--username", "a", "--trust-key", "abc"},
		"login plain http":      {"login", "--server", "http://localhost", "--username", "a"},
		"login no server":       {"login", "--username", "a"},
		"no skip flag":          {"login", "--server", "localhost", "--username", "a", "--insecure"},
		"read without login":    {"system", "--server", "localhost"},
	} {
		if _, _, err := skepctl(stateDir, nil, "", args...); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
	out, _, err := skepctl(stateDir, nil, "", "version")
	if err != nil || !strings.HasPrefix(out, "skepctl ") {
		t.Errorf("version: %v %q", err, out)
	}
}

// TestClean: control characters become spaces, format characters
// (category Cf) are removed (ADR 0010 amendment r), and other text is
// kept.
func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                     "plain text",
		"tab\tand\nnewline":              "tab and newline",
		"esc\x1b[31mred":                 "esc [31mred",
		"c1 \u009b csi":                  "c1   csi",
		"del\x7f":                        "del ",
		"bidi \u202egnp.exe":             "bidi gnp.exe",
		"\u202a\u202b\u202c\u202d\u202e": "",
		"\u2066\u2067\u2068\u2069 iso":   " iso",
		"zw\u200bsp\u200d\ufeff\u00ad":   "zwsp",
		"accented é and 日本":              "accented é and 日本",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Cf, r) {
			continue
		}
		if got := clean("a" + string(r) + "b"); got != "ab" {
			t.Fatalf("clean kept %U: %q", r, got)
		}
	}
}
