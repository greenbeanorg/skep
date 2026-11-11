// Package hardware performs read-only discovery of the host: CPU, memory,
// operating system, and the bhyve, QEMU, and ZFS capabilities.
//
// Discovery never changes host state. It reads kernel interfaces (sysctl on
// FreeBSD, procfs on Linux), checks whether files exist, and runs a small
// set of read-only commands with fixed argument lists. It never loads a
// kernel module, and it avoids commands that could load one as a side
// effect. See docs/decisions/0003-read-only-discovery.md.
//
// The platform logic lives in untagged files (freebsd.go, linux.go) behind
// small interfaces so it can be unit tested on any OS with fixtures. Only
// the thin bindings to the real system carry build tags.
package hardware

import (
	"context"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Discoverer performs one read-only discovery pass of the local host.
//
// A probe that fails does not fail the pass: the affected capability is
// reported as unknown or unavailable with a reason, and missing facts are
// listed in Hardware.Warnings. Discover returns an error only when the
// context is cancelled.
type Discoverer interface {
	Discover(ctx context.Context) (models.Hardware, error)
}

// CommandRunner runs one external command and returns its standard output.
// Implementations must not interpret the arguments through a shell.
type CommandRunner interface {
	Run(ctx context.Context, path string, args ...string) ([]byte, error)
}

// Options configures the platform Discoverer returned by New.
type Options struct {
	// CommandTimeout bounds each external command. Defaults to 5 seconds.
	CommandTimeout time.Duration
}

func (o Options) commandTimeout() time.Duration {
	if o.CommandTimeout <= 0 {
		return 5 * time.Second
	}
	return o.CommandTimeout
}

// pathFinder locates an executable by name and reports whether it exists.
type pathFinder func(name string) (string, bool)

// existsFunc reports whether a path exists.
type existsFunc func(path string) bool

// warnings collects non-fatal discovery problems.
type warnings []string

func (w *warnings) add(msg string) { *w = append(*w, msg) }

// list returns the warnings as a non-nil slice so JSON shows [] not null.
func (w warnings) list() []string {
	if w == nil {
		return []string{}
	}
	return w
}
