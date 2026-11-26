// Package e2e holds tests that run both halves of Skep together in one
// process: the agent's task engine, service layer, and internal API on a
// real Unix socket, and skep-api's client, identity stack, and public API
// in front of it, each on its own SQLite database.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agent"
	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clustertest"
	"github.com/greenbeanorg/skep/internal/console"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/internal/vm"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

const password = "correct horse battery staple"

type discoverer struct{}

func (discoverer) Discover(context.Context) (models.Hardware, error) {
	return models.Hardware{Platform: models.PlatformLinuxDev}, nil
}

type stack struct {
	agentDB *sql.DB
	apiDB   *sql.DB
	engine  *tasks.Engine
	handler http.Handler
	tokens  map[string]string
	userIDs map[string]string
	// The simulated host behind the agent's VM service, and its
	// confinement.
	fake *hostfake.Fake
	conf hostops.Confinement
	// noQuorum makes the API's quorum false (F5 of the 8d-1 review).
	noQuorum atomic.Bool
}

// agentServerTimeout is the agent server's per-request limit in these
// tests: short, so that a console that does not outlive it fails a test.
const agentServerTimeout = 2 * time.Second

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newStack wires an agent and an API together the way the two binaries
// do, with two task types on the agent: one that can be cancelled while it
// runs and one that cannot.
func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	// A short directory: the socket path is limited to about 104 bytes.
	dir, err := os.MkdirTemp("", "skep-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &stack{tokens: map[string]string{}, userIDs: map[string]string{}}

	// The agent.
	if s.agentDB, err = database.Open(ctx, filepath.Join(dir, "agent", "agent.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.agentDB.Close() })
	if _, err := database.Migrate(ctx, s.agentDB, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	nodeID, err := database.EnsureLocalNode(ctx, s.agentDB, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	s.engine, err = tasks.New(tasks.Options{
		Store: database.NewTaskStore(s.agentDB), Audit: database.NewAuditLog(s.agentDB),
		NodeID: nodeID, Workers: 2, QueueSize: 8, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(ctx context.Context, _ tasks.Task, report tasks.Reporter) (any, error) {
		report.Progress("waiting", nil)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for name, cancellable := range map[string]bool{"test.wait": true, "test.critical": false} {
		if err := s.engine.Register(tasks.Definition{Type: name, Cancellable: cancellable, Timeout: time.Minute, Run: wait}); err != nil {
			t.Fatal(err)
		}
	}
	// The VM service on the simulated host, as the agent wires it on a
	// development machine.
	s.conf = hostops.Confinement{
		Pools: []string{"devpool"}, DatasetRoot: "skep", MediaDir: filepath.Join(dir, "media"),
		Bridges: []string{"bridge0"}, ManagementInterfaces: []string{"vtnet0"},
		RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "agent", "vm"),
	}
	if err := os.MkdirAll(s.conf.MediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.conf.MediaDir, "install.iso"), []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.fake = hostfake.New(s.conf, hostfake.Options{PoolBytes: 20 << 30, HostCPUs: 4, HostMemoryBytes: 6 << 30, Logger: quiet(),
		Runner: func(ctx context.Context, ops hostops.Ops, vmID string, pid int, signals <-chan hostops.RunnerSignal) error {
			return bhyve.RunVM(ctx, ops, vmID, pid, signals, quiet())
		}})
	t.Cleanup(s.fake.Close)
	host := s.fake.Ops()
	consoles := console.New(console.Options{Open: host.VMM.OpenConsole, Audit: database.NewAuditLog(s.agentDB), Logger: quiet()})
	vms, err := vm.New(vm.Options{
		Console: consoles,
		NodeID:  nodeID, Store: database.NewVMStore(s.agentDB), Snapshots: database.NewSnapshotStore(s.agentDB), Engine: s.engine, Host: host, Logger: quiet(),
		Hypervisor: bhyve.New(bhyve.Options{Host: host, HostMemoryReserveBytes: 1 << 30, Logger: quiet()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range vms.Definitions() {
		if err := s.engine.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.engine.Shutdown(stop)
	})
	svc, err := agent.NewService(agent.Options{NodeID: nodeID, Discoverer: discoverer{}, Database: s.agentDB, Tasks: s.engine,
		Bhyve: vms.Capabilities, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "agent.sock")
	ln, err := internalapi.Listen(internalapi.ListenOptions{
		Path: socket, Mode: 0o600, GID: -1, AllowedUIDs: []uint32{uint32(os.Geteuid())}, MaxConnections: 8, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := internalapi.NewServer(internalapi.NewHandler(svc, internalapi.Options{VMs: vms, Console: consoles, RequestTimeout: agentServerTimeout, Logger: quiet()}), agentServerTimeout, quiet())
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); ln.Close() })

	// The API.
	if s.apiDB, err = database.Open(ctx, filepath.Join(dir, "api", "api.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.apiDB.Close() })
	if _, err := database.Migrate(ctx, s.apiDB, migrations.API()); err != nil {
		t.Fatal(err)
	}
	access := apidb.NewAccessLog(s.apiDB)
	hasher, err := auth.NewHasher(auth.MinParams, 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The users live in a real one-node skep-cluster (step 8c-1).
	ustack := clustertest.Start(t).Users(t, s.apiDB)
	store := ustack.Store
	users := auth.NewUsers(store, ustack.Writer, hasher, access, quiet())
	authn, err := auth.NewAuthenticator(auth.AuthenticatorOptions{
		Users: store, Writer: ustack.Writer, Sessions: apidb.NewSessionStore(s.apiDB), Hasher: hasher, Audit: access,
		Throttle: auth.DefaultThrottleConfig(),
		Session:  auth.SessionConfig{IdleTimeout: auth.DefaultIdleTimeout, AbsoluteTimeout: auth.DefaultAbsoluteTimeout},
		Logger:   quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := agentclient.New(agentclient.Options{SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	s.handler = api.NewHandler(api.Options{Backend: client, Auth: authn, Users: users, Audit: access, AuditLog: access, Logger: quiet(),
		ConsoleSessionCheck: 200 * time.Millisecond, Quorum: func() bool { return !s.noQuorum.Load() }})

	for _, role := range []string{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
		u, err := users.Create(ctx, auth.Actor{Name: "local:0"}, auth.NewUser{Username: "e2e-" + role, Password: password, RoleID: role})
		if err != nil {
			t.Fatal(err)
		}
		s.userIDs[role] = u.ID
		body, _ := json.Marshal(models.LoginRequest{Username: u.Username, Password: password, Session: models.SessionBearer})
		rec := s.call(http.MethodPost, "/api/v1/auth/login", "", body, "")
		var out models.SessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
			t.Fatalf("login as %s: %d %s", role, rec.Code, rec.Body)
		}
		s.tokens[role] = out.Token
	}
	return s
}

// call sends one request to the public API, as a client at 198.51.100.23.
func (s *stack) call(method, path, role string, body []byte, requestID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.23:40000"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+s.tokens[role])
	}
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// submit starts a task on the agent, as the system, and waits until it
// runs.
func (s *stack) submit(t *testing.T, taskType string) string {
	t.Helper()
	task, _, err := s.engine.Submit(context.Background(), tasks.Request{
		Type: taskType, Actor: models.TaskActor{ID: audit.System, Name: audit.System}, PeerUID: uint32(os.Geteuid()),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.waitStatus(t, task.ID, models.TaskRunning)
	return task.ID
}

func (s *stack) waitStatus(t *testing.T, id string, want models.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := s.engine.Get(context.Background(), id)
		if err == nil && task.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s is %s (%v), want %s", id, task.Status, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type agentAudit struct {
	ActorID, ActorName, Phase, Outcome, SourceIP, RequestID string
	PeerUID                                                 int64
}

// cancelAudit reads the agent's own audit entries about cancelling a task.
func (s *stack) cancelAudit(t *testing.T, taskID string) []agentAudit {
	t.Helper()
	rows, err := s.agentDB.Query(`SELECT actor_id, actor_name, peer_uid, phase, outcome, COALESCE(source_ip, ''), COALESCE(request_id, '')
		FROM audit_log WHERE action = 'task.cancel' AND task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []agentAudit
	for rows.Next() {
		var a agentAudit
		if err := rows.Scan(&a.ActorID, &a.ActorName, &a.PeerUID, &a.Phase, &a.Outcome, &a.SourceIP, &a.RequestID); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func (s *stack) count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCancelEndToEnd: a user cancels a running task through the public
// API. The agent stops the task and records, in its own audit log, the
// user as the actor, together with the peer UID it verified itself, the
// client's address, and the request ID that links both audit logs.
func TestCancelEndToEnd(t *testing.T) {
	s := newStack(t)
	id := s.submit(t, "test.wait")

	// A viewer is refused by the API. The agent never hears of it.
	rec := s.call(http.MethodPost, "/api/v1/tasks/"+id+"/cancel", auth.RoleViewer, nil, "req-viewer")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cancel as viewer: %d %s", rec.Code, rec.Body)
	}
	if got := s.cancelAudit(t, id); len(got) != 0 {
		t.Fatalf("a denied cancel reached the agent: %+v", got)
	}
	s.waitStatus(t, id, models.TaskRunning)
	if n := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'authz.deny' AND actor_id = ? AND request_id = 'req-viewer'`, s.userIDs[auth.RoleViewer]); n != 1 {
		t.Errorf("%d denials in the access audit log, want 1", n)
	}

	// An operator cancels it.
	rec = s.call(http.MethodPost, "/api/v1/tasks/"+id+"/cancel", auth.RoleOperator, nil, "req-e2e-cancel")
	var task models.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("cancel as operator: %d %s", rec.Code, rec.Body)
	}
	if task.ID != id || task.CancelRequestedAt == nil {
		t.Errorf("response = %+v, want the task with its cancel request recorded", task)
	}
	// The task was submitted by the system and stays the system's; the
	// canceller is recorded in the audit log, not by rewriting the task.
	if task.Actor.ID != audit.System {
		t.Errorf("task actor = %+v", task.Actor)
	}
	s.waitStatus(t, id, models.TaskCancelled)

	// The agent's record of who did it.
	got := s.cancelAudit(t, id)
	want := agentAudit{
		ActorID: s.userIDs[auth.RoleOperator], ActorName: "e2e-operator",
		PeerUID: int64(os.Geteuid()), Phase: "requested", Outcome: "success",
		SourceIP: "198.51.100.23", RequestID: "req-e2e-cancel",
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("agent audit = %+v\nwant one entry %+v", got, want)
	}
	// The API's record, with the same request ID.
	if n := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'task.cancel' AND outcome = 'success'
		AND actor_id = ? AND resource_id = ? AND request_id = 'req-e2e-cancel' AND source_ip = '198.51.100.23'`,
		s.userIDs[auth.RoleOperator], id); n != 1 {
		t.Errorf("%d cancel entries in the access audit log, want 1", n)
	}

	// The task's history, read back through the API by a viewer.
	rec = s.call(http.MethodGet, "/api/v1/tasks/"+id, auth.RoleViewer, nil, "")
	var detail models.TaskDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("read the task: %d %s", rec.Code, rec.Body)
	}
	if detail.Status != models.TaskCancelled || detail.Error == nil || detail.Error.Code != tasks.CodeCancelled {
		t.Errorf("task after the cancel = %+v", detail.Task)
	}

	// Cancelling a finished task changes nothing and is not an error.
	rec = s.call(http.MethodPost, "/api/v1/tasks/"+id+"/cancel", auth.RoleAdmin, nil, "")
	if rec.Code != http.StatusOK {
		t.Errorf("cancel of a finished task: %d %s", rec.Code, rec.Body)
	}
	if got := s.cancelAudit(t, id); len(got) != 1 {
		t.Errorf("a no-op cancel wrote to the agent's audit log: %+v", got)
	}
}

func TestCancelRefusalsEndToEnd(t *testing.T) {
	s := newStack(t)

	// A task whose type does not allow cancelling while it runs: the
	// agent refuses, whoever asks, and records the refusal.
	id := s.submit(t, "test.critical")
	rec := s.call(http.MethodPost, "/api/v1/tasks/"+id+"/cancel", auth.RoleAdmin, nil, "req-refused")
	var apiErr models.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &apiErr)
	if rec.Code != http.StatusConflict || apiErr.Error.Code != models.CodeNotCancellable {
		t.Fatalf("cancel of a non-cancellable task: %d %s", rec.Code, rec.Body)
	}
	s.waitStatus(t, id, models.TaskRunning)
	got := s.cancelAudit(t, id)
	if len(got) != 1 || got[0].Outcome != "denied" || got[0].ActorID != s.userIDs[auth.RoleAdmin] || got[0].RequestID != "req-refused" {
		t.Errorf("agent audit of the refusal = %+v", got)
	}

	// Unknown task, no session.
	if rec := s.call(http.MethodPost, "/api/v1/tasks/00000000-0000-4000-8000-000000000000/cancel", auth.RoleAdmin, nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown task: %d", rec.Code)
	}
	if rec := s.call(http.MethodPost, "/api/v1/tasks/"+id+"/cancel", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("without a session: %d", rec.Code)
	}
	if n := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'task.cancel'`); n != 1 {
		t.Errorf("%d cancel entries in the agent's audit log, want only the refusal", n)
	}
}
