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
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", c, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
