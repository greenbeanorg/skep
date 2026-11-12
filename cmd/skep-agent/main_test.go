package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
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
	// The startup inventory refresh ran as a task, submitted by "system".
	waitTask := func(wantTotal int) models.Task {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			list, err := client.Tasks(ctx, models.TaskListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) == wantTotal && list.Items[0].Status.Terminal() {
				return list.Items[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("tasks = %+v", list)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	refresh := waitTask(1)
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
	// refresh instead of touching the old one.
	second := waitTask(2)
	if second.ID == refresh.ID || second.Status != models.TaskSucceeded {
		t.Errorf("second startup task = %+v", second)
	}
}
