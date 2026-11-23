package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "skep-agent "+version.Version) {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":      {"-nope"},
		"stray argument":    {"extra"},
		"missing config":    {"-config", "/nonexistent/skep.json"},
		"the removed flag":  {"-dev", "-listen", "127.0.0.1:0", "-data-dir", t.TempDir()},
		"unknown peer user": {"-dev", "-data-dir", t.TempDir(), "-config", writeConfig(t, `{"socket":{"allowed_users":["skep-no-such-user"]}}`)},
		"invalid log level": {"-dev", "-log-level", "loud", "-data-dir", t.TempDir()},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, &stdout, &stderr); err == nil {
			t.Errorf("%s: run succeeded, want an error", name)
		}
	}
}

// syncBuffer is a bytes.Buffer that is safe to write from the agent while
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// shortDir returns a directory with a short path: the agent socket lives
// in it, and sun_path is limited to about 104 bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "skep-agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestRunServesInternalAPI boots the whole agent with the real discoverer
// and a real database, calls every read operation over its socket, restarts
// it, and checks that the node ID is stable across the restart.
func TestRunServesInternalAPI(t *testing.T) {
	dir := filepath.Join(shortDir(t), "data")
	socket := filepath.Join(dir, "run", "agent.sock")

	boot := func() (stop func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		logs := &syncBuffer{}
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"-dev", "-data-dir", dir, "-log-format", "json"}, io.Discard, logs)
		}()

		deadline := time.Now().Add(15 * time.Second)
		for !strings.Contains(logs.String(), `"msg":"internal API listening"`) {
			select {
			case err := <-done:
				cancel()
				t.Fatalf("agent exited early: %v\n%s", err, logs.String())
			default:
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("agent did not start listening:\n%s", logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		// The agent must not have a network listener of any kind.
		if strings.Contains(logs.String(), `"address"`) {
			cancel()
			t.Fatalf("the agent logged a network address:\n%s", logs.String())
		}
		return func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("agent did not stop cleanly: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("agent did not stop")
			}
		}
	}

	client, err := agentclient.New(agentclient.Options{SocketPath: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := agentproto.WithCaller(context.Background(), agentproto.Caller{ActorID: "test", ActorName: "test"})

	stop := boot()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("dev socket: %v, %v; want mode 0600", info, err)
	}
	health := client.Health(ctx)
	if health.Status != models.HealthOK || health.Checks["database"].Status != models.HealthOK {
		t.Errorf("health = %+v", health)
	}
	// The audit archive is one of the agent's health checks.
	if c, ok := health.Checks[models.CheckAuditArchive]; !ok || c.Status != models.HealthOK {
		t.Errorf("audit archive health check = %+v (present %v)", c, ok)
	}
	sys, err := client.System(ctx)
	if err != nil || sys.NodeID == "" || !sys.DevMode || sys.ProductionHost {
		t.Fatalf("system = %+v, %v", sys, err)
	}
	hw, err := client.Hardware(ctx, sys.NodeID)
	if err != nil || hw.NodeID != sys.NodeID || hw.CPU.Threads < 1 || hw.Memory.TotalBytes == 0 {
		t.Errorf("hardware = %+v, %v", hw, err)
	}
	for _, c := range []models.Capability{hw.Hypervisors.Bhyve.Capability, hw.Hypervisors.QEMU.Capability, hw.Storage.ZFS.Capability} {
		if c.Status == "" || c.Reason == "" {
			t.Errorf("capability without status or reason: %+v", c)
		}
	}
	// Every start submits two tasks as "system": the inventory refresh and
	// the retention run. waitTasks waits until starts of each have finished
	// and returns the newest of each type.
	waitTasks := func(starts int) map[string]models.Task {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			list, err := client.Tasks(ctx, models.TaskListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			newest, done := map[string]models.Task{}, true
			for _, task := range list.Items { // newest first
				done = done && task.Status.Terminal()
				if _, seen := newest[task.Type]; !seen {
					newest[task.Type] = task
				}
			}
			if done && len(list.Items) == 2*starts && len(newest) == 2 {
				return newest
			}
			if time.Now().After(deadline) {
				t.Fatalf("tasks = %+v", list)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first := waitTasks(1)
	refresh := first["discovery.refresh"]
	if refresh.Type != "discovery.refresh" || refresh.Status != models.TaskSucceeded ||
		refresh.Actor.ID != "system" || refresh.ResourceID != sys.NodeID {
		t.Errorf("startup task = %+v", refresh)
	}
	detail, err := client.Task(ctx, refresh.ID)
	if err != nil || detail.Result == nil {
		t.Errorf("task detail has no result: %+v, %v", detail, err)
	}
	if len(detail.Events) < 3 || detail.Events[0].Message != "task is pending" ||
		detail.Events[len(detail.Events)-1].Message != "task is succeeded" {
		t.Errorf("task events = %+v", detail.Events)
	}
	// The retention run: a task of its own, with the system as its actor,
	// whose result says what it removed. Nothing is old enough here.
	kept := first["maintenance.retention"]
	if kept.Status != models.TaskSucceeded || kept.Actor.ID != "system" || kept.Cancellable ||
		kept.ResourceType != "maintenance" || kept.ResourceID != "retention" {
		t.Errorf("retention task = %+v", kept)
	}
	keptDetail, err := client.Task(ctx, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Tasks struct {
			RetentionDays int    `json:"retention_days"`
			Cutoff        string `json:"cutoff"`
			TasksRemoved  *int   `json:"tasks_removed"`
		} `json:"tasks"`
		Audit *struct {
			Verified *int `json:"segments_verified"`
		} `json:"audit"`
	}
	raw, err := json.Marshal(keptDetail.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("retention result %s: %v", raw, err)
	}
	if result.Tasks.RetentionDays != 90 || result.Tasks.Cutoff == "" || result.Tasks.TasksRemoved == nil ||
		*result.Tasks.TasksRemoved != 0 || result.Audit == nil || result.Audit.Verified == nil {
		t.Errorf("retention result = %s", raw)
	}
	if _, err := client.Task(ctx, "unknown"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown task: %v", err)
	}
	if _, err := client.Hardware(ctx, "unknown"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown node: %v", err)
	}
	var invalid *models.Error
	if _, err := client.Tasks(ctx, models.TaskListOptions{Limit: 100000}); !errors.As(err, &invalid) || invalid.Code != models.CodeInvalidRequest {
		t.Errorf("invalid limit: %v", err)
	}
	stop()
	if _, err := os.Stat(socket); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket was not removed at shutdown: %v", err)
	}
	if _, err := client.System(ctx); !errors.Is(err, agentclient.ErrUnavailable) {
		t.Errorf("a stopped agent: %v, want ErrUnavailable", err)
	}

	stop = boot()
	defer stop()
	again, err := client.System(ctx)
	if err != nil || again.NodeID != sys.NodeID {
		t.Errorf("node ID changed across a restart: %q then %q (%v)", sys.NodeID, again.NodeID, err)
	}
	// Task history survives the restart, and the new start adds its own
	// tasks instead of touching the old ones.
	second := waitTasks(2)
	for typ, task := range second {
		if task.ID == first[typ].ID || task.Status != models.TaskSucceeded {
			t.Errorf("second %s task = %+v", typ, task)
		}
	}
}

type observerFunc func(context.Context) error

func (f observerFunc) ObserveAll(ctx context.Context) error { return f(ctx) }

// TestObserveContainersShutdown: a shutdown during the observation is a
// normal stop, with no warning; any other failure is logged and the start
// goes on (ADR 0010 amendment aa, rule 10).
func TestObserveContainersShutdown(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !observeContainers(ctx, observerFunc(func(context.Context) error { cancel(); return context.Canceled }), log) {
		t.Fatal("a cancelled observation did not report a shutdown")
	}
	if logs.Len() != 0 {
		t.Fatalf("a shutdown logged: %s", logs.String())
	}
	if observeContainers(context.Background(), observerFunc(func(context.Context) error { return errors.New("boom") }), log) {
		t.Fatal("an ordinary failure reported a shutdown")
	}
	if !strings.Contains(logs.String(), "could not observe the jails") {
		t.Fatalf("log: %s", logs.String())
	}
}
