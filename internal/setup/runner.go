package setup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Cmd is one command setup runs: a program and its arguments, never a
// shell line.
type Cmd struct {
	Name string
	Args []string
	// Interactive connects the operator's terminal (skep-api admin
	// create asks for a password).
	Interactive bool
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Runner runs commands. The host runner executes them; the tests use a
// fake that emulates sysrc on the fixture files.
type Runner interface {
	Run(ctx context.Context, c Cmd) (string, error)
}

// HostRunner executes commands with exec.Command.
type HostRunner struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Run implements Runner.
func (h HostRunner) Run(ctx context.Context, c Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	if c.Interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = h.Stdin, h.Stdout, h.Stderr
		return "", cmd.Run()
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", c, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
