package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
		"non-loopback bind": {"-dev", "-listen", "0.0.0.0:0", "-data-dir", t.TempDir()},
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

// TestRunServesAPI boots the whole agent on an ephemeral loopback port with
// the real discoverer and a real database, calls every endpoint, restarts
// it, and checks that the node ID is stable across the restart.
func TestRunServesAPI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	boot := func() (base string, stop func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		logs := &syncBuffer{}
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"-dev", "-listen", "127.0.0.1:0", "-data-dir", dir, "-log-format", "json"}, io.Discard, logs)
		}()

		addrRE := regexp.MustCompile(`"msg":"API listening","address":"([^"]+)"`)
		deadline := time.Now().Add(15 * time.Second)
		for base == "" {
			select {
			case err := <-done:
				cancel()
				t.Fatalf("agent exited early: %v\n%s", err, logs.String())
			default:
			}
			if m := addrRE.FindStringSubmatch(logs.String()); m != nil {
				base = "http://" + m[1]
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("agent did not start listening:\n%s", logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		return base, func() {
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

	get := func(url string, wantStatus int, into any) {
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

	base, stop := boot()
	var health models.Health
	get(base+"/api/v1/health", http.StatusOK, &health)
	if health.Status != models.HealthOK || health.Checks["database"].Status != models.HealthOK {
		t.Errorf("health = %+v", health)
	}
	var sys models.System
	get(base+"/api/v1/system", http.StatusOK, &sys)
	if sys.NodeID == "" || !sys.DevMode || sys.ProductionHost {
		t.Errorf("system = %+v", sys)
	}
	var hw models.Hardware
	get(base+"/api/v1/nodes/"+sys.NodeID+"/hardware", http.StatusOK, &hw)
	if hw.NodeID != sys.NodeID || hw.CPU.Threads < 1 || hw.Memory.TotalBytes == 0 {
		t.Errorf("hardware = %+v", hw)
	}
	for _, c := range []models.Capability{hw.Hypervisors.Bhyve.Capability, hw.Hypervisors.QEMU.Capability, hw.Storage.ZFS.Capability} {
		if c.Status == "" || c.Reason == "" {
			t.Errorf("capability without status or reason: %+v", c)
		}
	}
	var apiErr models.ErrorResponse
	get(base+"/api/v1/nodes/unknown/hardware", http.StatusNotFound, &apiErr)
	if apiErr.Error.Code != "not_found" {
		t.Errorf("error = %+v", apiErr)
	}
	stop()

	base, stop = boot()
	defer stop()
	var again models.System
	get(base+"/api/v1/system", http.StatusOK, &again)
	if again.NodeID != sys.NodeID {
		t.Errorf("node ID changed across a restart: %q then %q", sys.NodeID, again.NodeID)
	}
}
