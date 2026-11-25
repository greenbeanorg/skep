//go:build !skep_testcluster

package main

import (
	"context"
	"fmt"
)

// testClusterBuilt reports whether this binary has the test-only
// "test-init" command (the skep_testcluster build tag). It never has in a
// shipped build.
const testClusterBuilt = false

func runTestInit(context.Context, []string, env) error {
	return fmt.Errorf("unknown command %q\n%s", "test-init", usage)
}
