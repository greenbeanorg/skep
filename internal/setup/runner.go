package setup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// The base-system programs setup runs as root, by absolute path, so the
// caller's PATH never chooses them (FreeBSD 15 locations).
const (
	cmdService        = "/usr/sbin/service"
	cmdSysrc          = "/usr/sbin/sysrc"
	cmdPw             = "/usr/sbin/pw"
	cmdFstat          = "/usr/bin/fstat"
	cmdZfs            = "/sbin/zfs"
	cmdZpool          = "/sbin/zpool"
	cmdIfconfig       = "/sbin/ifconfig"
	cmdJls            = "/usr/sbin/jls"
	cmdSysctl         = "/sbin/sysctl"
	cmdKldstat        = "/sbin/kldstat"
	cmdKenv           = "/bin/kenv"
	cmdFreeBSDVersion = "/bin/freebsd-version"
)

// commandPath is the PATH every command setup runs gets; rc.d scripts and
// the programs they call need one.
const commandPath = "PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"

// CommandEnv is the fixed environment of every command setup runs:
// nothing of the caller's environment except TERM, for an interactive
// command that reads a password from the terminal.
func CommandEnv(interactive bool) []string {
	env := []string{commandPath, "HOME=/"}
	if t := os.Getenv("TERM"); interactive && t != "" && !strings.ContainsAny(t, "\x00\n") {
		env = append(env, "TERM="+t)
	}
	return env
}

// Credential is the user a command runs as instead of root.
type Credential struct {
	UID, GID uint32
}

// Cmd is one command setup runs: a program and its arguments, never a
// shell line.
type Cmd struct {
	Name string
	Args []string
	// Interactive connects the operator's terminal (skep-api admin
	// create asks for a password).
	Interactive bool
	// As, when set, runs the command as that user with no supplementary
	// groups.
	As *Credential
	// Limit, when above 0, is the most of the standard output that is
	// kept; the rest is read and dropped, so the command is not blocked.
	Limit int
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Runner runs commands. The host runner executes them; the tests use a
// fake that emulates sysrc on the fixture files.
type Runner interface {
	Run(ctx context.Context, c Cmd) (string, error)
}

// HostRunner executes commands with exec.Command, with CommandEnv's
// environment. A command that is not interactive runs in its own process
// group, so the terminal's Ctrl-C reaches setup, which decides what to
// cancel, and not a "service start" half way through.
type HostRunner struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Run implements Runner.
func (h HostRunner) Run(ctx context.Context, c Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = CommandEnv(c.Interactive)
	cmd.Dir = "/"
	attr, err := sysProcAttr(c)
	if err != nil {
		return "", err
	}
	cmd.SysProcAttr = attr
	if c.Interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = h.Stdin, h.Stdout, h.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%s: %w", c, err)
		}
		return "", nil
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &limitWriter{w: &errb, n: 64 << 10}
	if c.Limit > 0 {
		cmd.Stdout = &limitWriter{w: &out, n: c.Limit}
	}
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(errb.String()); tail != "" {
			return out.String(), fmt.Errorf("%s: %w: %s", c, err, tail)
		}
		return out.String(), fmt.Errorf("%s: %w", c, err)
	}
	return out.String(), nil
}

// limitWriter keeps the first n bytes written to it and drops the rest,
// reporting every write as complete.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
		l.n -= k
	}
	return len(p), nil
}
