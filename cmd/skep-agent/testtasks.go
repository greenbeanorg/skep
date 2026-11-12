//go:build !skep_testtasks

package main

import (
	"context"
	"log/slog"

	"github.com/greenbeanorg/skep/internal/tasks"
)

// testTasksBuilt reports whether this binary contains the test-only task
// types. It is false in every shipped build: they exist only in a binary
// built with the skep_testtasks tag, for native integration tests.
const testTasksBuilt = false

// registerTestTasks registers nothing in a shipped build.
func registerTestTasks(*tasks.Engine) error { return nil }

// submitTestTasks submits nothing in a shipped build.
func submitTestTasks(context.Context, *tasks.Engine, *slog.Logger) {}
