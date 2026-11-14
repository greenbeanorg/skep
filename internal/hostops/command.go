package hostops

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The only binaries this package runs. Fixed paths: the environment of a
// root process must not decide what gets executed.
const (
	zfsBinary      = "/sbin/zfs"
	zpoolBinary    = "/sbin/zpool"
	ifconfigBinary = "/sbin/ifconfig"
	kldstatBinary  = "/sbin/kldstat"
	bhyveBinary    = "/usr/sbin/bhyve"
	bhyvectlBinary = "/usr/sbin/bhyvectl"
)

// Runner runs one command and returns its standard output. It must not
// interpret the arguments through a shell. A command that ran and failed
// is reported as a *CommandError.
type Runner interface {
	Run(ctx context.Context, path string, args ...string) ([]byte, error)
}

// CommandError is a command that exited with a failure.
type CommandError struct {
	// Name is the base name of the binary.
	Name     string
	ExitCode int
	// Stderr is the first line of what the command wrote to stderr.
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("%s: %v: %s", e.Name, e.Err, e.Stderr)
	}
	return fmt.Sprintf("%s: %v", e.Name, e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

// stderrContains reports whether err is a failed command whose message
// contains text.
func stderrContains(err error, text string) bool {
	var ce *CommandError
	return errors.As(err, &ce) && strings.Contains(ce.Stderr, text)
}
