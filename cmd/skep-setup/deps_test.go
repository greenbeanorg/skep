package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// V1 of step 8c-2 (D1, D21): neither skep-setup nor skep-api links the Raft
// library; they learn about the cluster through skep-cluster's commands and
// its socket. Checked for the development build and for FreeBSD.
func TestNoRaftLibraryLinked(t *testing.T) {
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gobin); err != nil {
		if gobin, err = exec.LookPath("go"); err != nil {
			t.Fatal("no go command to list the dependencies with")
		}
	}
	for _, goos := range []string{runtime.GOOS, "freebsd"} {
		for _, cmd := range []string{"../skep-setup", "../skep-api"} {
			c := exec.Command(gobin, "list", "-deps", cmd)
			c.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("go list -deps %s (GOOS=%s): %v\n%s", cmd, goos, err, out)
			}
			for _, dep := range strings.Fields(string(out)) {
				if strings.HasPrefix(dep, "github.com/hashicorp/raft") || dep == "github.com/greenbeanorg/skep/internal/cluster/store" ||
					dep == "github.com/greenbeanorg/skep/internal/cluster/localapi" {
					t.Errorf("%s (GOOS=%s) links %s", cmd, goos, dep)
				}
			}
		}
	}
}
