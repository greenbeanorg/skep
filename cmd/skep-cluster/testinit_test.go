//go:build !skep_testcluster

package main

import (
	"context"
	"strings"
	"testing"
)

// A shipped skep-cluster has no test-init: it is not a command at all.
func TestTestInitIsNotInTheDefaultBuild(t *testing.T) {
	if testClusterBuilt {
		t.Fatal("the default build claims the test-only command")
	}
	w := newWorld(t)
	err := run(context.Background(), []string{"test-init", "-config", "x", "-data-dir", "y"}, w.env(1000))
	if err == nil || !strings.Contains(err.Error(), `unknown command "test-init"`) {
		t.Fatalf("test-init in the default build: %v", err)
	}
}
