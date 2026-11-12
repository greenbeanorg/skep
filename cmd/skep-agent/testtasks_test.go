//go:build !skep_testtasks

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestShippedBuildHasNoTestTasks guards the default build: the test-only
// task types must not be compiled in, whatever the environment says.
func TestShippedBuildHasNoTestTasks(t *testing.T) {
	if testTasksBuilt {
		t.Fatal("the default build contains the test-only task types")
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "TEST BUILD") {
		t.Errorf("version output = %q", stdout.String())
	}
}
