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
	// EnvCommand.StderrLastLine. The first line of jail -c and of a
	// command in a template build jail is cleaned too (cleanStderr,
	// amendment r).
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
// line: control characters (C0, DEL, and C1), format characters
// (category Cf, among them the bidirectional overrides and isolates
// U+202A–U+202E and U+2066–U+2069, which can make a line display in an
// order other than its own; amendment q), and bytes that are not UTF-8
// (which a terminal may read as C1 controls) are removed, the surrounding
// spaces trimmed, and the result cut to at most limit bytes on a
// character boundary.
func cleanLine(s string, limit int) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if (r == utf8.RuneError && size <= 1) || unsafeRune(r) {
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

// unsafeRune reports whether r is a control character (C0, DEL, or C1)
// or a format character (category Cf): the characters Skep removes from
// jail output it reports (amendments q and r).
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// cleanStderr runs the stderr line of a failed command through cleanLine
// (amendment r): for commands whose stderr the jail's side can write, a
// command in a template build jail, or jail(8) creating a jail, reported
// in a task error. Any other error is returned as it is.
func cleanStderr(err error) error {
	var ce *CommandError
	if errors.As(err, &ce) {
		ce.Stderr = cleanLine(ce.Stderr, maxStderrLine)
	}
	return err
}

// CleanText removes format characters (category Cf) and control
// characters (C0, DEL, and C1) from text a jail wrote, keeping newlines
// and tabs, so that a client that shows it, a browser's <pre> or a
// terminal, cannot be made to reorder it or move the cursor (amendment
// r). A byte that is not UTF-8 becomes U+FFFD, as JSON encoding would
// make it anyway. The text is not trimmed or cut.
func CleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unsafeRune(r) {
			return -1
		}
		return r
	}, s)
}
