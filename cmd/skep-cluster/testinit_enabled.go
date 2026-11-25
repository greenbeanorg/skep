//go:build skep_testcluster

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/ids"
)

// This file is compiled only with the skep_testcluster build tag, the way
// the agent's test tasks are (cmd/skep-agent/testtasks_enabled.go). It
// adds "test-init": a one-node cluster without root, for the browser
// test's stack (scripts/e2e-stack.sh; step 8c-1, D22). It must never be
// part of a shipped skep-cluster: the default build compiles testinit.go.

// testClusterBuilt reports that this binary has the test-only command.
const testClusterBuilt = true

// runTestInit creates a one-node cluster in a data directory the caller
// owns, without a listener and without asking skep-agent for the node ID
// (a new one is made). It refuses to run as root.
func runTestInit(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster test-init", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	configPath := flags.String("config", "", "path to cluster.json to write")
	dataDir := flags.String("data-dir", "", "data directory to create (mode 0700)")
	name := flags.String("name", "e2e", "node name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if e.euid == 0 {
		return errors.New("test-init is for tests and never runs as root")
	}
	if *configPath == "" || *dataDir == "" {
		return errors.New("test-init needs -config and -data-dir")
	}
	if err := os.Mkdir(*dataDir, 0o700); err != nil {
		return err
	}
	id, err := ids.NewUUID()
	if err != nil {
		return err
	}
	cfg := clusterconf.Config{NodeID: id, NodeName: *name}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := clusterconf.Write(*configPath, cfg, -1, -1); err != nil {
		return err
	}
	clusterID, _, err := service.CreateCluster(ctx, *dataDir, cfg, "", time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "test cluster %s, node %s\n", clusterID, id)
	return nil
}
