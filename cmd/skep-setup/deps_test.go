package main

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// V1 of step 8c-2 (D1, D21): neither skep-setup nor skep-api links the Raft
// library; they learn about the cluster through skep-cluster's commands and
// its socket. Checked for the development build and for FreeBSD. Step
// 8d-1 (D24): skep-api reaches the guest inventory through package
// guestapi only, never skep-cluster's service, peer or socket packages.
func TestNoRaftLibraryLinked(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("no go command to list the dependencies with")
	}
	for _, goos := range []string{runtime.GOOS, "freebsd"} {
		for _, cmd := range []string{"../skep-setup", "../skep-api"} {
			c := exec.Command(gobin, "list", "-deps", cmd)
			c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps %s (GOOS=%s): %v\n%s", cmd, goos, err, out)
			}
			deps := strings.Fields(string(out))
			for _, dep := range deps {
				switch {
				case strings.HasPrefix(dep, "github.com/hashicorp/raft"),
					dep == "github.com/greenbeanorg/skep/internal/cluster/store",
					dep == "github.com/greenbeanorg/skep/internal/cluster/localapi",
					dep == "github.com/greenbeanorg/skep/internal/cluster/service",
					dep == "github.com/greenbeanorg/skep/internal/cluster/raftnet":
					t.Errorf("%s (GOOS=%s) links %s", cmd, goos, dep)
				}
			}
			if cmd == "../skep-api" && !slices.Contains(deps, "github.com/greenbeanorg/skep/internal/cluster/guestapi") {
				t.Errorf("skep-api (GOOS=%s) does not link guestapi: the guest inventory is not wired", goos)
			}
		}
	}
}
