package internalapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/peercred"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeBackend records the context of the last call, so tests can check
// what the handlers pass down.
type fakeBackend struct {
	mu       sync.Mutex
	caller   agentproto.Caller
	peer     peercred.Cred
	hadPeer  bool
	taskOpts models.TaskListOptions
	err      error
}

func (f *fakeBackend) record(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caller, _ = agentproto.CallerFrom(ctx)
	f.peer, f.hadPeer = internalapi.PeerFrom(ctx)
}

func (f *fakeBackend) last() (agentproto.Caller, peercred.Cred, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caller, f.peer, f.hadPeer
}

func (f *fakeBackend) Health(ctx context.Context) models.Health {
	f.record(ctx)
	return models.Health{Status: models.HealthOK, Version: "test", UptimeSeconds: 7,
		Checks: map[string]models.HealthCheck{"database": {Status: models.HealthOK}}}
}

func (f *fakeBackend) System(ctx context.Context) (models.System, error) {
	f.record(ctx)
	return models.System{NodeID: "node-1", Hostname: "hearst"}, f.err
}

func (f *fakeBackend) Hardware(ctx context.Context, id string) (models.Hardware, error) {
	f.record(ctx)
	if id != "node-1" {
		return models.Hardware{}, fmt.Errorf("node %q: %w", id, models.ErrNotFound)
	}
	return models.Hardware{NodeID: id}, nil
}

func (f *fakeBackend) Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	f.record(ctx)
	f.mu.Lock()
	f.taskOpts = opts
	f.mu.Unlock()
	if opts.Limit > 500 {
		return models.TaskList{}, models.Invalid("limit must be between 1 and 500")
	}
	return models.TaskList{Items: []models.Task{{ID: "task-1"}}, NextCursor: "next"}, nil
}

func (f *fakeBackend) Task(ctx context.Context, id string) (models.TaskDetail, error) {
	f.record(ctx)
	if id != "task-1" {
		return models.TaskDetail{}, models.ErrNotFound
	}
	return models.TaskDetail{Task: models.Task{ID: id}, Events: []models.TaskEvent{}}, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// socketDir returns a short directory: sun_path is limited to about 104
// bytes, and t.TempDir() can be longer than that.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "skep-ia")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// start serves the internal API on a fresh socket and returns its path.
func start(t *testing.T, backend internalapi.Backend, allowed []uint32) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "agent.sock")
	ln, err := internalapi.Listen(internalapi.ListenOptions{
		Path: path, Mode: 0o600, GID: -1, AllowedUIDs: allowed, MaxConnections: 8, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := internalapi.NewServer(internalapi.NewHandler(backend, internalapi.Options{
		RequestTimeout: 5 * time.Second, Logger: quiet(),
	}), 5*time.Second, quiet())
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return path
}

func me() []uint32 { return []uint32{uint32(os.Geteuid())} }

func newClient(t *testing.T, path string) *agentclient.Client {
	t.Helper()
	c, err := agentclient.New(agentclient.Options{SocketPath: path, ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

var testCaller = agentproto.Caller{ActorID: "user-1", ActorName: "andy", RequestID: "req-42", SourceIP: "192.0.2.7"}

func callerCtx() context.Context {
	return agentproto.WithCaller(context.Background(), testCaller)
}

func TestReadsThroughTheSocket(t *testing.T) {
	backend := &fakeBackend{}
	c := newClient(t, start(t, backend, me()))
	ctx := callerCtx()

	h := c.Health(ctx)
	if h.Status != models.HealthOK || h.Version != "test" || h.Checks["database"].Status != models.HealthOK ||
		h.Checks["agent"].Status != models.HealthOK {
		t.Errorf("health = %+v", h)
	}
	sys, err := c.System(ctx)
	if err != nil || sys.NodeID != "node-1" {
		t.Errorf("system = %+v, %v", sys, err)
	}
	hw, err := c.Hardware(ctx, "node-1")
	if err != nil || hw.NodeID != "node-1" {
		t.Errorf("hardware = %+v, %v", hw, err)
	}
	list, err := c.Tasks(ctx, models.TaskListOptions{Status: models.TaskRunning, Limit: 5, Cursor: "abc"})
	if err != nil || len(list.Items) != 1 || list.NextCursor != "next" {
		t.Errorf("tasks = %+v, %v", list, err)
	}
	if backend.taskOpts != (models.TaskListOptions{Status: models.TaskRunning, Limit: 5, Cursor: "abc"}) {
		t.Errorf("task options were not passed through: %+v", backend.taskOpts)
	}
	detail, err := c.Task(ctx, "task-1")
	if err != nil || detail.ID != "task-1" {
		t.Errorf("task = %+v, %v", detail, err)
	}

	caller, peer, ok := backend.last()
	if caller != testCaller {
		t.Errorf("the backend saw caller %+v, want %+v", caller, testCaller)
	}
	if !ok || peer.UID != uint32(os.Geteuid()) {
		t.Errorf("the backend saw peer %+v (present %v), want this process's UID", peer, ok)
	}
}

func TestServiceErrorsCrossTheSocket(t *testing.T) {
	backend := &fakeBackend{}
	c := newClient(t, start(t, backend, me()))
	ctx := callerCtx()

	if _, err := c.Hardware(ctx, "other"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown node: %v, want ErrNotFound", err)
	}
	if _, err := c.Task(ctx, "nope"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown task: %v, want ErrNotFound", err)
	}
	_, err := c.Tasks(ctx, models.TaskListOptions{Limit: 501})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeInvalidRequest || !strings.Contains(me.Message, "limit") {
		t.Errorf("invalid request: %v", err)
	}
	// An internal error of the agent stays internal: no code for the client.
	backend.err = errors.New("disk on fire")
	_, err = c.System(ctx)
	if err == nil || errors.As(err, &me) || strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("internal error: %v", err)
	}
}

// raw sends a request over the socket without the client's help.
func raw(t *testing.T, path, method, url string, hdr http.Header) (*http.Response, models.ErrorBody) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://agent"+url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env models.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp, env.Error
}

func TestEveryRouteRequiresAnActor(t *testing.T) {
	backend := &fakeBackend{}
	path := start(t, backend, me())
	routes := []string{"/health", "/system", "/nodes/node-1/hardware", "/tasks", "/tasks/task-1", "/nope"}
	for _, route := range routes {
		for name, hdr := range map[string]http.Header{
			"no headers": {},
			"id only":    {agentproto.HeaderActorID: {"user-1"}},
			"name only":  {agentproto.HeaderActorName: {"andy"}},
			"bad actor":  {agentproto.HeaderActorID: {"user 1"}, agentproto.HeaderActorName: {"andy"}},
			"bad source": {agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}, agentproto.HeaderSourceIP: {"nope"}},
		} {
			resp, body := raw(t, path, http.MethodGet, agentproto.Prefix+route, hdr)
			if resp.StatusCode != http.StatusBadRequest || body.Code != "invalid_request" {
				t.Errorf("%s, %s: status %d code %q, want 400 invalid_request", route, name, resp.StatusCode, body.Code)
			}
		}
	}
	if _, _, called := backend.last(); called {
		t.Error("a handler ran for a request without an actor")
	}
}

func TestInternalRoutesAreReadOnlyAndClosed(t *testing.T) {
	path := start(t, &fakeBackend{}, me())
	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}, agentproto.HeaderRequestID: {"req-9"}}

	resp, body := raw(t, path, http.MethodPost, agentproto.Prefix+"/system", hdr)
	if resp.StatusCode != http.StatusMethodNotAllowed || body.Code != "method_not_allowed" {
		t.Errorf("POST: status %d code %q", resp.StatusCode, body.Code)
	}
	// The request ID sent by the API is the one the agent uses.
	if body.RequestID != "req-9" || resp.Header.Get("X-Request-ID") != "req-9" {
		t.Errorf("request ID = %q / %q, want req-9", body.RequestID, resp.Header.Get("X-Request-ID"))
	}
	// The public paths do not exist on the socket.
	for _, p := range []string{"/api/v1/system", "/internal/v2/system", "/"} {
		if resp, body := raw(t, path, http.MethodGet, p, hdr); resp.StatusCode != http.StatusNotFound || body.Code != "not_found" {
			t.Errorf("GET %s: status %d code %q", p, resp.StatusCode, body.Code)
		}
	}
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/nodes/bad%20id/hardware", hdr); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid node ID: status %d", resp.StatusCode)
	}
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/tasks?limit=x", hdr); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid limit: status %d", resp.StatusCode)
	}
}

func TestPeerOutsideTheAllowListIsDisconnected(t *testing.T) {
	backend := &fakeBackend{}
	// Everyone but this process.
	path := start(t, backend, []uint32{uint32(os.Geteuid()) + 1})
	c := newClient(t, path)

	_, err := c.System(callerCtx())
	if !errors.Is(err, agentclient.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if h := c.Health(callerCtx()); h.Status != models.HealthDegraded || h.Checks["agent"].Status != models.HealthDegraded {
		t.Errorf("health = %+v, want degraded", h)
	}
	if _, _, called := backend.last(); called {
		t.Error("a handler ran for a peer outside the allow-list")
	}
}

func TestHandlerRefusesUnverifiedPeer(t *testing.T) {
	backend := &fakeBackend{}
	h := internalapi.NewHandler(backend, internalapi.Options{Logger: quiet()})
	req := httptest.NewRequest(http.MethodGet, agentproto.Prefix+"/system", nil)
	testCaller.SetHeaders(req.Header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: the handler was not served through the agent's listener", rec.Code)
	}
	if _, _, called := backend.last(); called {
		t.Error("a handler ran without a verified peer")
	}
}

func TestListenSocketFile(t *testing.T) {
	dir := socketDir(t)
	path := filepath.Join(dir, "agent.sock")
	opts := internalapi.ListenOptions{Path: path, Mode: 0o660, GID: os.Getegid(), AllowedUIDs: me(), MaxConnections: 1, Logger: quiet()}

	ln, err := internalapi.Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSocket == 0 || info.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, want a socket with 0660", info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the temporary socket name was left behind: %v", entries)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Close left the socket file: %v", err)
	}

	// A stale socket from a crashed agent is replaced.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	ln, err = internalapi.Listen(opts)
	if err != nil {
		t.Fatalf("a stale socket must be replaced: %v", err)
	}
	ln.Close()

	// Anything else at the path is left alone.
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := internalapi.Listen(opts); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a regular file at the socket path: %v", err)
	}
}

func TestListenValidation(t *testing.T) {
	base := internalapi.ListenOptions{Path: filepath.Join(socketDir(t), "s"), Mode: 0o600, GID: -1, AllowedUIDs: me(), MaxConnections: 1}
	for name, mutate := range map[string]func(*internalapi.ListenOptions){
		"empty path":       func(o *internalapi.ListenOptions) { o.Path = "" },
		"long path":        func(o *internalapi.ListenOptions) { o.Path = "/tmp/" + strings.Repeat("x", 100) },
		"empty allow-list": func(o *internalapi.ListenOptions) { o.AllowedUIDs = nil },
		"no connections":   func(o *internalapi.ListenOptions) { o.MaxConnections = 0 },
		"missing dir":      func(o *internalapi.ListenOptions) { o.Path = "/nonexistent-skep/agent.sock" },
	} {
		o := base
		mutate(&o)
		if ln, err := internalapi.Listen(o); err == nil {
			ln.Close()
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConnectionLimit(t *testing.T) {
	path := filepath.Join(socketDir(t), "agent.sock")
	ln, err := internalapi.Listen(internalapi.ListenOptions{Path: path, Mode: 0o600, GID: -1, AllowedUIDs: me(), MaxConnections: 1, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	dial()
	first := <-accepted
	dial()
	select {
	case <-accepted:
		t.Fatal("a second connection was accepted above the limit")
	case <-time.After(100 * time.Millisecond):
	}
	first.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting connection was not accepted after the first closed")
	}
}
