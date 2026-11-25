//go:build skep_testcluster

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// The test build creates a one-node cluster without root, and refuses to
// run as root.
func TestTestInitCreatesAClusterWithoutRoot(t *testing.T) {
	if !testClusterBuilt {
		t.Fatal("the test build does not claim the test-only command")
	}
	w := newWorld(t)
	dir := t.TempDir()
	cfg, data := filepath.Join(dir, "cluster.json"), filepath.Join(dir, "data")
	if err := run(context.Background(), []string{"test-init", "-config", cfg, "-data-dir", data}, w.env(0)); err == nil {
		t.Fatal("test-init ran as root")
	}
	if err := run(context.Background(), []string{"test-init", "-config", cfg, "-data-dir", data}, w.env(1000)); err != nil {
		t.Fatal(err)
	}
	c, err := clusterconf.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Exists(layout.State(data)) || c.ListenAddress != "" {
		t.Fatalf("no store, or a listener: %+v", c)
	}
}
