//go:build freebsd

package hostops

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// TestPSNative runs the real ps(1) with BhyveUsage's arguments on this
// test process: three columns, a resident size and a CPU time that parse,
// and a command name that is not bhyve, which parsePS refuses as not
// found. On FreeBSD a single "-o rss=,time=,comm=" is one column whose
// header is ",time=,comm="; this test is what catches that.
func TestPSNative(t *testing.T) {
	out, err := execRunner{timeout: 10 * time.Second}.Run(context.Background(), psBinary, psArgs(os.Getpid())...)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(firstLine(out))
	if len(f) != 3 {
		t.Fatalf("ps printed %q, want three columns", out)
	}
	if _, err := parseCPUTime(f[1]); err != nil {
		t.Errorf("CPU time %q: %v", f[1], err)
	}
	if _, err := parsePS(out); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("parsePS of a process that is not bhyve: %v", err)
	}
	t.Logf("ps: %q", strings.TrimSpace(string(out)))
}
