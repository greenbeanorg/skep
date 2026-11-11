//go:build linux

package hardware

import (
	"os"
	"runtime"
	"time"
)

// New returns the Linux development-mode Discoverer. Linux is never a
// production Skep host; this exists so the agent can run on a dev machine.
func New(opts Options) Discoverer {
	return &linuxDiscoverer{
		root: os.DirFS("/"),
		arch: runtime.GOARCH,
		find: findInDirs(searchDirs),
		run:  execRunner{timeout: opts.commandTimeout()},
		now:  time.Now,
	}
}
