package hostops

import (
	"errors"
	"log/slog"
	"sync"
	"syscall"
	"time"
)

// errNoSuchOID is returned by system when a sysctl OID does not exist,
// which usually means a kernel module is not loaded.
var errNoSuchOID = errors.New("sysctl oid not found")

// system is what the implementation reads from the kernel and the file
// system besides running commands. The FreeBSD binding is in
// sys_freebsd.go; tests use a fake.
type system interface {
	// SysctlUint64 reads an integer sysctl of any width.
	SysctlUint64(name string) (uint64, error)
	// SysctlExists reports whether an OID exists, whatever its type.
	SysctlExists(name string) (bool, error)
	// Exists reports whether a path exists, without following a final
	// symbolic link.
	Exists(path string) bool
	// Alive reports whether a process exists.
	Alive(pid int) bool
	// ProcessArgs returns a process's argument list.
	ProcessArgs(pid int) ([]string, error)
	// Kill sends a signal to a process.
	Kill(pid int, sig syscall.Signal) error
}

// Options configures the real implementation.
type Options struct {
	// CommandTimeout bounds each command. Defaults to 30 seconds.
	CommandTimeout time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Self is the absolute path of the agent's own executable, which
	// runs the per-VM runners. Empty means no runner can be started.
	Self string
}

// host is the real implementation of every interface in this package.
type host struct {
	conf  Confinement
	run   Runner
	sys   system
	spawn spawner
	self  string
	log   *slog.Logger

	// ignoredMedia remembers which entries of the media directory were
	// already logged as ignored.
	mediaMu      sync.Mutex
	ignoredMedia map[string]bool
}

func newHost(conf Confinement, run Runner, sys system, log *slog.Logger) *host {
	if log == nil {
		log = slog.Default()
	}
	return &host{conf: conf, run: run, sys: sys, log: log, ignoredMedia: map[string]bool{}}
}

// NewFiles returns the Files implementation alone. It is plain file I/O
// under the configured directories and works on any OS; the development
// fake uses it for the media directory and the per-VM state.
func NewFiles(conf Confinement, log *slog.Logger) Files {
	return newHost(conf, nil, nil, log)
}
