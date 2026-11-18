package hostops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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
	// Stderr is the first line of what the command wrote to stderr, or
	// the last, cleaned by cleanLine, for a command run with
	// EnvCommand.StderrLastLine.
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

// maxStderrLine caps the stderr line a failed jail -r reports
// (amendment p).
const maxStderrLine = 200

// lastLine is the last non-empty line of b, without its line ending.
func lastLine(b []byte) string {
	s := strings.TrimRight(string(b), "\r\n\t ")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// cleanLine makes text a process wrote safe to put in an error and a log
// line: control characters (C0, DEL, and C1) and bytes that are not
// UTF-8 (which a terminal may read as C1 controls) are removed, the
// surrounding spaces trimmed, and the result cut to at most limit bytes
// on a character boundary.
func cleanLine(s string, limit int) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if (r == utf8.RuneError && size <= 1) || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}
