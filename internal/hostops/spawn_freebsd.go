//go:build freebsd

package hostops

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// execSpawner starts long-lived children: an absolute path, an explicit
// argument list, a fixed minimal environment, no stdin, and no shell.
type execSpawner struct{}

func (execSpawner) Spawn(path string, args []string, opts spawnOptions) (Process, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("hostops: refusing to run non-absolute command %q", path)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: opts.NewSession}
	if opts.Output != "" {
		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if opts.Truncate {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		out, err := os.OpenFile(opts.Output, flags, 0o600)
		if err != nil {
			return nil, fmt.Errorf("hostops: open %s: %w", opts.Output, err)
		}
		defer out.Close() // the child keeps its own descriptor
		cmd.Stdout, cmd.Stderr = out, out
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd}, nil
}

type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) PID() int { return p.cmd.Process.Pid }

func (p *execProcess) Signal(sig syscall.Signal) error { return p.cmd.Process.Signal(sig) }

func (p *execProcess) Wait() ExitStatus {
	err := p.cmd.Wait()
	if err == nil {
		return ExitStatus{Code: 0}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ExitStatus{Code: -1, Signal: ws.Signal().String()}
		}
		return ExitStatus{Code: exit.ExitCode()}
	}
	return ExitStatus{Code: -1, Err: err}
}
