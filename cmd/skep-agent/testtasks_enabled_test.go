//go:build skep_testtasks

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/tasks"
)

type nopReporter struct{}

func (nopReporter) Progress(string, any) {}

func TestSleepPayload(t *testing.T) {
	for _, bad := range []string{`{}`, `{"duration":"x"}`, `{"duration":"0s"}`, `{"duration":"-1s"}`, `{"duration":"10m"}`} {
		if _, err := parseSleep(json.RawMessage(bad)); err == nil {
			t.Errorf("parseSleep(%s) succeeded", bad)
		}
	}
	if d, err := parseSleep(json.RawMessage(`{"duration":"90s"}`)); err != nil || d != 90*time.Second {
		t.Errorf("parseSleep = %v, %v", d, err)
	}
}

func TestSleepRunsAndStopsOnCancel(t *testing.T) {
	task := tasks.Task{Payload: json.RawMessage(`{"duration":"20ms"}`)}
	if _, err := runTestSleep(context.Background(), task, nopReporter{}); err != nil {
		t.Fatalf("short sleep: %v", err)
	}

	task.Payload = json.RawMessage(`{"duration":"5m"}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runTestSleep(ctx, task, nopReporter{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sleep task ignored cancellation")
	}
}

func TestTestBuildIsLabelled(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "TEST BUILD") {
		t.Errorf("version output = %q", stdout.String())
	}
}
