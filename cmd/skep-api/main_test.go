package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

// testEnv is an unprivileged environment with captured output.
func testEnv(stdout, stderr io.Writer) env {
	return env{
		stdin: strings.NewReader(""), stdout: stdout, stderr: stderr, uid: 1002, euid: 1002,
		drop: func(uint32, uint32) error { return errors.New("the test environment is not root") },
		readPassword: func(context.Context, string) (string, error) {
			return "", errors.New("the test environment has no terminal")
		},
	}
}

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	e := testEnv(&stdout, io.Discard)
	e.euid = 0 // printing the version is allowed for anyone
	if err := run(context.Background(), []string{"-version"}, e); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "skep-api "+version.Version) {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestRunRefusesRoot(t *testing.T) {
	e := testEnv(io.Discard, io.Discard)
	e.euid = 0
	err := run(context.Background(), []string{"-dev", "-data-dir", t.TempDir(), "-listen", "127.0.0.1:0"}, e)
	if !errors.Is(err, errRoot) {
		t.Fatalf("error = %v, want a refusal to run as root", err)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":       {"-nope"},
		"stray argument":     {"extra"},
		"missing config":     {"-config", "/nonexistent/api.json"},
		"bad listen address": {"-dev", "-listen", "nope", "-data-dir", t.TempDir()},
		"invalid log level":  {"-dev", "-log-level", "loud", "-data-dir", t.TempDir()},
	} {
		if err := run(context.Background(), args, testEnv(io.Discard, io.Discard)); err == nil {
			t.Errorf("%s: run succeeded, want an error", name)
		}
	}
}

// syncBuffer is a bytes.Buffer that is safe to write from the server while
// the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	// stop stops the server that writes to this buffer.
	stop func()
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeAgent is the agent's service layer for the socket the API talks to.
type fakeAgent struct {
	mu     sync.Mutex
	caller agentproto.Caller
}

func (f *fakeAgent) seen() agentproto.Caller {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caller
}

func (f *fakeAgent) Health(context.Context) models.Health {
	return models.Health{Status: models.HealthOK, Version: "test", Checks: map[string]models.HealthCheck{"database": {Status: models.HealthOK}}}
}

func (f *fakeAgent) System(ctx context.Context) (models.System, error) {
	f.mu.Lock()
	f.caller, _ = agentproto.CallerFrom(ctx)
	f.mu.Unlock()
	return models.System{NodeID: "node-1", Hostname: "hearst", Platform: models.PlatformFreeBSD}, nil
}

func (f *fakeAgent) Hardware(_ context.Context, id string) (models.Hardware, error) {
	if id != "node-1" {
		return models.Hardware{}, models.ErrNotFound
	}
	return models.Hardware{NodeID: id}, nil
}

func (f *fakeAgent) Tasks(context.Context, models.TaskListOptions) (models.TaskList, error) {
	return models.TaskList{Items: []models.Task{{ID: "task-1"}}}, nil
}

func (f *fakeAgent) Task(_ context.Context, id string) (models.TaskDetail, error) {
	if id != "task-1" {
		return models.TaskDetail{}, models.ErrNotFound
	}
	return models.TaskDetail{Task: models.Task{ID: id}, Events: []models.TaskEvent{}}, nil
}

func (f *fakeAgent) CancelTask(_ context.Context, id string, req tasks.CancelRequest) (models.Task, error) {
	if id != "task-1" {
		return models.Task{}, models.ErrNotFound
	}
	return models.Task{ID: id, Status: models.TaskCancelled, Actor: req.Actor}, nil
}

// startAgent serves a fake agent on a socket and returns a function that
// stops it.
func startAgent(t *testing.T, socket string, agent *fakeAgent) (stop func()) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ln, err := internalapi.Listen(internalapi.ListenOptions{
		Path: socket, Mode: 0o600, GID: -1, AllowedUIDs: []uint32{uint32(os.Geteuid())}, MaxConnections: 8, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := internalapi.NewServer(internalapi.NewHandler(agent, internalapi.Options{Logger: quiet}), 5*time.Second, quiet)
	go srv.Serve(ln)
	return func() {
		srv.Close()
		ln.Close()
	}
}

// startAPI runs skep-api on an ephemeral loopback port and returns its
// base URL. Use client to talk to it.
func startAPI(t *testing.T, args ...string) (base string) {
	t.Helper()
	base, _ = startAPIWithLogs(t, args...)
	return base
}

// startAPIWithLogs is startAPI that also returns the server's log.
func startAPIWithLogs(t *testing.T, args ...string) (base string, logs *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logs = &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, append([]string{"-listen", "127.0.0.1:0", "-log-format", "json"}, args...), testEnv(io.Discard, logs))
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("skep-api did not stop cleanly: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("skep-api did not stop")
		}
	}
	t.Cleanup(stop)
	logs.stop = stop

	addrRE := regexp.MustCompile(`"msg":"API listening","address":"([^"]+)"`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("skep-api exited early: %v\n%s", err, logs.String())
		default:
		}
		if m := addrRE.FindStringSubmatch(logs.String()); m != nil {
			return "https://" + m[1], logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("skep-api did not start listening:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// client returns an HTTP client that trusts exactly the certificate
// skep-api generated in dataDir, as a careful administrator would.
func client(t *testing.T, dataDir string) *http.Client {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join(dataDir, "api", "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("the generated certificate is not PEM")
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 10 * time.Second}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// bearer adds a session token to every request of a client.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// createAdmin creates the administrator "andy" with the local command, as
// an operator would after installing.
func createAdmin(t *testing.T, cfg, dataDir string) {
	t.Helper()
	e := testEnv(io.Discard, io.Discard)
	e.stdin = strings.NewReader(testPassword + "\n")
	if err := run(context.Background(), []string{"admin", "create", "--username", "andy", "--password-stdin", "-dev", "-config", cfg, "-data-dir", dataDir}, e); err != nil {
		t.Fatal(err)
	}
}

// login logs "andy" in for a bearer session and returns a client that
// sends its token.
func login(t *testing.T, c *http.Client, base string) *http.Client {
	t.Helper()
	body, _ := json.Marshal(models.LoginRequest{Username: "andy", Password: testPassword, Session: models.SessionBearer})
	resp, err := c.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out models.SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("login: status %d, %v", resp.StatusCode, err)
	}
	return withToken(c, out.Token)
}

func withToken(c *http.Client, token string) *http.Client {
	authed := &http.Client{Transport: bearer{token: token, next: c.Transport}, Timeout: c.Timeout}
	return authed
}

func get(t *testing.T, c *http.Client, url string, wantStatus int, into any) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d", url, resp.StatusCode, wantStatus)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v", url, err)
	}
	return resp
}

// TestRunServesAPIThroughTheSocket runs the real skep-api against an agent
// socket: the public API answers from what the agent says, keeps answering
// while the agent is away, and recovers when it comes back.
func TestRunServesAPIThroughTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "skep-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")

	// The API starts first: the start order must not matter.
	dataDir, cfg := filepath.Join(dir, "data"), fastConfig(t)
	createAdmin(t, cfg, dataDir)
	base := startAPI(t, "-dev", "-config", cfg, "-data-dir", dataDir, "-agent-socket", socket)
	anonymous := client(t, dataDir)

	// Without a session: the health status and nothing else.
	var status map[string]any
	get(t, anonymous, base+"/api/v1/health", http.StatusServiceUnavailable, &status)
	if len(status) != 1 || status["status"] != models.HealthDegraded {
		t.Errorf("unauthenticated health = %v, want only the status", status)
	}
	var denied models.ErrorResponse
	get(t, anonymous, base+"/api/v1/system", http.StatusUnauthorized, &denied)
	if denied.Error.Code != "unauthenticated" {
		t.Errorf("unauthenticated system = %+v", denied)
	}
	// Logging in works while the agent is down.
	c := login(t, anonymous, base)

	var health models.Health
	get(t, c, base+"/api/v1/health", http.StatusServiceUnavailable, &health)
	if health.Status != models.HealthDegraded || health.Checks["agent"].Status != models.HealthDegraded {
		t.Errorf("health without an agent = %+v", health)
	}
	var apiErr models.ErrorResponse
	get(t, c, base+"/api/v1/system", http.StatusServiceUnavailable, &apiErr)
	if apiErr.Error.Code != models.CodeAgentUnavailable {
		t.Errorf("system without an agent = %+v", apiErr)
	}

	agent := &fakeAgent{}
	stopAgent := startAgent(t, socket, agent)
	get(t, c, base+"/api/v1/health", http.StatusOK, &health)
	if health.Status != models.HealthOK || health.Checks["agent"].Status != models.HealthOK ||
		health.Checks["database"].Status != models.HealthOK {
		t.Errorf("health = %+v", health)
	}
	var sys models.System
	get(t, c, base+"/api/v1/system", http.StatusOK, &sys)
	if sys.NodeID != "node-1" || sys.Hostname != "hearst" {
		t.Errorf("system = %+v", sys)
	}
	// The agent is told which user the request is for.
	if c := agent.seen(); c.ActorName != "andy" || len(c.ActorID) != 36 || c.RequestID == "" || c.SourceIP != "127.0.0.1" {
		t.Errorf("the agent saw caller %+v", c)
	}
	var hw models.Hardware
	get(t, c, base+"/api/v1/nodes/node-1/hardware", http.StatusOK, &hw)
	var list models.TaskList
	get(t, c, base+"/api/v1/tasks", http.StatusOK, &list)
	var detail models.TaskDetail
	get(t, c, base+"/api/v1/tasks/task-1", http.StatusOK, &detail)
	if hw.NodeID != "node-1" || len(list.Items) != 1 || detail.ID != "task-1" {
		t.Errorf("hardware %+v, tasks %+v, task %+v", hw, list, detail)
	}
	get(t, c, base+"/api/v1/tasks/unknown", http.StatusNotFound, &apiErr)
	get(t, c, base+"/api/v1/nodes/unknown/hardware", http.StatusNotFound, &apiErr)
	if apiErr.Error.Code != "not_found" {
		t.Errorf("error = %+v", apiErr)
	}

	// The agent goes away and comes back; the API does neither.
	stopAgent()
	get(t, c, base+"/api/v1/system", http.StatusServiceUnavailable, &apiErr)
	defer startAgent(t, socket, agent)()
	get(t, c, base+"/api/v1/system", http.StatusOK, &sys)
}

// TestRunTLS checks the listener itself: TLS 1.3 only, no plaintext, the
// generated certificate with private files, no HSTS for a self-signed
// certificate, and the certificate reported in the system document.
func TestRunTLS(t *testing.T) {
	dir, err := os.MkdirTemp("", "skep-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	defer startAgent(t, socket, &fakeAgent{})()
	dataDir, cfg := filepath.Join(dir, "data"), fastConfig(t)
	createAdmin(t, cfg, dataDir)
	base := startAPI(t, "-dev", "-config", cfg, "-data-dir", dataDir, "-agent-socket", socket)
	addr := strings.TrimPrefix(base, "https://")

	for path, want := range map[string]os.FileMode{
		filepath.Join(dataDir, "api"):                   0o700,
		filepath.Join(dataDir, "api", "api.db"):         0o600,
		filepath.Join(dataDir, "api", "tls"):            0o700,
		filepath.Join(dataDir, "api", "tls", "key.pem"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want mode %04o", path, info, err, want)
		}
	}

	// Plain HTTP gets no API response.
	resp, err := http.Get("http://" + addr + "/api/v1/health")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("the API answered over plain HTTP")
		}
	}
	// TLS 1.2 is refused.
	if conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}); err == nil {
		conn.Close()
		t.Error("a TLS 1.2 handshake succeeded")
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	state := conn.ConnectionState()
	conn.Close()
	if state.Version != tls.VersionTLS13 {
		t.Errorf("negotiated TLS version %x", state.Version)
	}

	c := login(t, client(t, dataDir), base)
	token := c.Transport.(bearer).token
	var sys models.System
	resp = get(t, c, base+"/api/v1/system", http.StatusOK, &sys)
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("Strict-Transport-Security = %q with a self-signed certificate", hsts)
	}
	leaf := state.PeerCertificates[0]
	if sys.API == nil || sys.API.TLS.Source != "self-signed" || !sys.API.TLS.NotAfter.Equal(leaf.NotAfter) ||
		!strings.HasPrefix(sys.API.TLS.PublicKeySHA256, "sha256:") {
		t.Errorf("system.api = %+v", sys.API)
	}

	// "tls fingerprint" prints the same pin, without a running server
	// being involved.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"tls", "fingerprint", "-dev", "-data-dir", dataDir}, testEnv(&out, io.Discard)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), sys.API.TLS.PublicKeySHA256) || !strings.Contains(out.String(), "self-signed") {
		t.Errorf("tls fingerprint printed:\n%s\nwant the pin %s", out.String(), sys.API.TLS.PublicKeySHA256)
	}

	// "tls regenerate" makes a new key; SIGHUP makes the running server
	// serve it at once; the change is in the access audit log.
	out.Reset()
	e := testEnv(&out, io.Discard)
	e.uid = 4242
	if err := run(context.Background(), []string{"tls", "regenerate", "-dev", "-data-dir", dataDir}, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "old public key: "+sys.API.TLS.PublicKeySHA256) {
		t.Errorf("tls regenerate printed:\n%s", out.String())
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	var after models.System
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The session survives the key change; only the trust anchor is new.
		get(t, withToken(client(t, dataDir), token), base+"/api/v1/system", http.StatusOK, &after)
		if after.API.TLS.PublicKeySHA256 != sys.API.TLS.PublicKeySHA256 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the running server did not pick up the regenerated key after SIGHUP")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), after.API.TLS.PublicKeySHA256) {
		t.Errorf("the server's new pin %s is not the one regenerate printed:\n%s", after.API.TLS.PublicKeySHA256, out.String())
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "api", "api.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var actor, details string
	if err := db.QueryRow(`SELECT actor_name, details FROM audit_log WHERE action = 'tls.key.regenerate'`).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "local:4242" || !strings.Contains(details, sys.API.TLS.PublicKeySHA256) || !strings.Contains(details, after.API.TLS.PublicKeySHA256) {
		t.Errorf("audit entry: actor %q, details %s", actor, details)
	}
}

func TestTLSCommandErrors(t *testing.T) {
	dataDir := t.TempDir()
	for name, args := range map[string][]string{
		"no command":            {"tls"},
		"unknown command":       {"tls", "rotate"},
		"stray argument":        {"tls", "fingerprint", "-dev", "-data-dir", dataDir, "extra"},
		"nothing generated yet": {"tls", "fingerprint", "-dev", "-data-dir", dataDir},
	} {
		if err := run(context.Background(), args, testEnv(io.Discard, io.Discard)); err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}

// TestLocalCommandsDropRoot: a local command started as root gives up root
// for the owner of the API's state directory before it opens anything.
func TestLocalCommandsDropRoot(t *testing.T) {
	dataDir := t.TempDir()

	// The state directory does not exist: nothing to drop to, so stop.
	e := testEnv(io.Discard, io.Discard)
	e.euid = 0
	dropped := false
	e.drop = func(uint32, uint32) error { dropped = true; return nil }
	err := run(context.Background(), []string{"tls", "regenerate", "-dev", "-data-dir", dataDir}, e)
	if err == nil || !strings.Contains(err.Error(), "does not exist yet") || dropped {
		t.Errorf("missing state directory: %v (dropped=%v)", err, dropped)
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
		t.Errorf("a root command created files before dropping privileges: %v", entries)
	}

	// The state directory exists and belongs to this user: the command
	// drops to exactly that owner, then works.
	if err := os.Mkdir(filepath.Join(dataDir, "api"), 0o700); err != nil {
		t.Fatal(err)
	}
	var gotUID, gotGID uint32
	e.drop = func(uid, gid uint32) error { gotUID, gotGID, dropped = uid, gid, true; return nil }
	if err := run(context.Background(), []string{"tls", "regenerate", "-dev", "-data-dir", dataDir}, e); err != nil {
		t.Fatal(err)
	}
	if !dropped || gotUID != uint32(os.Getuid()) || gotGID != uint32(os.Getgid()) {
		t.Errorf("dropped=%v to %d:%d, want the directory owner %d:%d", dropped, gotUID, gotGID, os.Getuid(), os.Getgid())
	}

	// If root cannot be given up, nothing is opened.
	other := t.TempDir()
	os.Mkdir(filepath.Join(other, "api"), 0o700)
	e.drop = func(uint32, uint32) error { return errors.New("setuid failed") }
	err = run(context.Background(), []string{"tls", "regenerate", "-dev", "-data-dir", other}, e)
	if err == nil || !strings.Contains(err.Error(), "cannot give up root") {
		t.Errorf("failed drop: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(other, "api")); len(entries) != 0 {
		t.Errorf("files were created as root: %v", entries)
	}
}

// TestOperatorCertificateIsServedWithHSTS runs the server with an
// operator-supplied pair.
func TestOperatorCertificateIsServedWithHSTS(t *testing.T) {
	dir := t.TempDir()
	// Use a generated pair as the "operator's" files.
	gen := filepath.Join(dir, "gen")
	if err := run(context.Background(), []string{"tls", "regenerate", "-dev", "-data-dir", gen}, testEnv(io.Discard, io.Discard)); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(gen, "api", "tls", "cert.pem"), filepath.Join(gen, "api", "tls", "key.pem")
	cfgFile := filepath.Join(dir, "api.json")
	cfg, _ := json.Marshal(map[string]any{"tls": map[string]string{"cert_file": certFile, "key_file": keyFile}})
	if err := os.WriteFile(cfgFile, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	base := startAPI(t, "-dev", "-config", cfgFile, "-data-dir", dataDir, "-agent-socket", filepath.Join(dir, "absent.sock"))

	var health models.HealthStatus
	resp := get(t, client(t, gen), base+"/api/v1/health", http.StatusServiceUnavailable, &health)
	if hsts := resp.Header.Get("Strict-Transport-Security"); !strings.HasPrefix(hsts, "max-age=") {
		t.Errorf("Strict-Transport-Security = %q with an operator certificate", hsts)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "api", "tls")); err == nil {
		t.Error("a certificate was generated although one is configured")
	}
	// Skep never replaces operator files.
	err := run(context.Background(), []string{"tls", "regenerate", "-dev", "-config", cfgFile, "-data-dir", dataDir}, testEnv(io.Discard, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "operator-supplied") {
		t.Errorf("tls regenerate with an operator certificate: %v", err)
	}
}
