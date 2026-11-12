//go:build skep_testtasks

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// This file is compiled only with the skep_testtasks build tag. It adds a
// slow task type so that native integration tests can interrupt a task
// that is really in flight. It must never be part of a shipped agent: the
// default build compiles testtasks.go instead.

// testTasksBuilt reports that this binary contains the test-only tasks.
const testTasksBuilt = true

const (
	// taskTestSleep does nothing for the requested time. It touches
	// nothing on the host.
	taskTestSleep = "test.sleep"
	// envTestSleep makes the agent submit one test.sleep task at start,
	// as the system actor, for the given duration (for example "2m").
	// Tasks cannot be submitted over the network yet, so this is the only
	// way to get one in flight.
	envTestSleep = "SKEP_TEST_SLEEP"
	// maxTestSleep bounds the duration and is the task type's time limit.
	maxTestSleep = 10 * time.Minute
)

type sleepPayload struct {
	Duration string `json:"duration"`
}

func parseSleep(payload json.RawMessage) (time.Duration, error) {
	var p sleepPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, errors.New("payload must be {\"duration\": \"30s\"}")
	}
	d, err := time.ParseDuration(p.Duration)
	if err != nil || d <= 0 || d >= maxTestSleep {
		return 0, fmt.Errorf("duration must be positive and less than %s", maxTestSleep)
	}
	return d, nil
}

func registerTestTasks(engine *tasks.Engine) error {
	return engine.Register(tasks.Definition{
		Type:        taskTestSleep,
		Cancellable: true,
		Timeout:     maxTestSleep,
		Validate: func(payload json.RawMessage) error {
			_, err := parseSleep(payload)
			return err
		},
		Run: runTestSleep,
	})
}

// runTestSleep waits for the requested time and reports progress once a
// second, so an interrupted task has events showing it was in flight.
func runTestSleep(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	d, err := parseSleep(task.Payload)
	if err != nil {
		return nil, &tasks.Failure{Code: tasks.CodeFailed, Message: err.Error()}
	}
	start := time.Now()
	report.Progress("sleeping", map[string]any{"duration": d.String()})
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	done := time.NewTimer(d)
	defer done.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done.C:
			return map[string]any{"slept": d.String()}, nil
		case <-tick.C:
			report.Progress("still sleeping", map[string]any{"elapsed": time.Since(start).Round(time.Second).String()})
		}
	}
}

func submitTestTasks(ctx context.Context, engine *tasks.Engine, log *slog.Logger) {
	log.Warn("this agent is a TEST BUILD with test-only task types; never install it on a real host")
	v := os.Getenv(envTestSleep)
	if v == "" {
		return
	}
	payload, _ := json.Marshal(sleepPayload{Duration: v})
	task, _, err := engine.Submit(ctx, tasks.Request{
		Type:    taskTestSleep,
		Payload: payload,
		Actor:   models.TaskActor{ID: audit.System, Name: audit.System},
		PeerUID: uint32(os.Geteuid()),
	})
	if err != nil {
		log.Error("could not submit the test sleep task", "error", err)
		return
	}
	log.Warn("submitted a test sleep task", "task_id", task.ID, "duration", v)
}
