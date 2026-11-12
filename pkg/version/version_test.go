package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version == "" || info.Commit == "" || info.BuildDate == "" {
		t.Fatalf("build metadata must never be empty: %+v", info)
	}
	if info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("unexpected platform: %+v", info)
	}
	if !strings.Contains(info.String(), info.Version) {
		t.Fatalf("String() %q does not contain the version", info.String())
	}
}
