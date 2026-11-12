package main

import (
	"bytes"
	"context"
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
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

// testEnv is an unprivileged environment with captured output.
func testEnv(stdout, stderr io.Writer) env {
	return env{stdout: stdout, stderr: stderr, euid: 1002}
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
		"unknown flag":      {"-nope"},
		"stray argument":    {"extra"},
		"missing config":    {"-config", "/nonexistent/api.json"},
		"non-loopback bind": {"-dev", "-listen", "0.0.0.0:0", "-data-dir", t.TempDir()},
		"invalid log level": {"-dev", "-log-level", "loud", "-data-dir", t.TempDir()},
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
// base URL.
func startAPI(t *testing.T, args ...string) (base string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, append([]string{"-listen", "127.0.0.1:0", "-log-format", "json"}, args...), testEnv(io.Discard, logs))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("skep-api did not stop cleanly: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("skep-api did not stop")
		}
	})

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
			return "http://" + m[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("skep-api did not start listening:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string, wantStatus int, into any) {
	t.Helper()
	resp, err := http.Get(url)
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
	base := startAPI(t, "-dev", "-data-dir", filepath.Join(dir, "data"), "-agent-socket", socket)

	var health models.Health
	get(t, base+"/api/v1/health", http.StatusServiceUnavailable, &health)
	if health.Status != models.HealthDegraded || health.Checks["agent"].Status != models.HealthDegraded {
		t.Errorf("health without an agent = %+v", health)
	}
	var apiErr models.ErrorResponse
	get(t, base+"/api/v1/system", http.StatusServiceUnavailable, &apiErr)
	if apiErr.Error.Code != models.CodeAgentUnavailable {
		t.Errorf("system without an agent = %+v", apiErr)
	}

	agent := &fakeAgent{}
	stopAgent := startAgent(t, socket, agent)
	get(t, base+"/api/v1/health", http.StatusOK, &health)
	if health.Status != models.HealthOK || health.Checks["agent"].Status != models.HealthOK ||
		health.Checks["database"].Status != models.HealthOK {
		t.Errorf("health = %+v", health)
	}
	var sys models.System
	get(t, base+"/api/v1/system", http.StatusOK, &sys)
	if sys.NodeID != "node-1" || sys.Hostname != "hearst" {
		t.Errorf("system = %+v", sys)
	}
	if c := agent.seen(); c.ActorID != "anonymous" || c.RequestID == "" || c.SourceIP != "127.0.0.1" {
		t.Errorf("the agent saw caller %+v", c)
	}
	var hw models.Hardware
	get(t, base+"/api/v1/nodes/node-1/hardware", http.StatusOK, &hw)
	var list models.TaskList
	get(t, base+"/api/v1/tasks", http.StatusOK, &list)
	var detail models.TaskDetail
	get(t, base+"/api/v1/tasks/task-1", http.StatusOK, &detail)
	if hw.NodeID != "node-1" || len(list.Items) != 1 || detail.ID != "task-1" {
		t.Errorf("hardware %+v, tasks %+v, task %+v", hw, list, detail)
	}
	get(t, base+"/api/v1/tasks/unknown", http.StatusNotFound, &apiErr)
	get(t, base+"/api/v1/nodes/unknown/hardware", http.StatusNotFound, &apiErr)
	if apiErr.Error.Code != "not_found" {
		t.Errorf("error = %+v", apiErr)
	}

	// The agent goes away and comes back; the API does neither.
	stopAgent()
	get(t, base+"/api/v1/system", http.StatusServiceUnavailable, &apiErr)
	defer startAgent(t, socket, agent)()
	get(t, base+"/api/v1/system", http.StatusOK, &sys)
}
