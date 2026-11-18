//go:build freebsd

package hostops

import (
	"context"
	"time"
)

// execRunner is the real Runner: an absolute path, an explicit argument
// list, a fixed minimal environment, no stdin, a timeout, capped output.
// The process handling is in exec_unix.go.
type execRunner struct {
	timeout time.Duration
}

func (r execRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return runCommand(ctx, r.timeout, maxOutput, overflowCut,
		[]string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}, path, args...)
}

// RunEnv implements EnvRunner: the same rules as Run, with the command's
// own complete environment, timeout, and output cap.
func (r execRunner) RunEnv(ctx context.Context, c EnvCommand) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	if c.ConsoleLog != "" {
		return nil, runToConsoleLog(ctx, timeout, c)
	}
	limit := c.MaxOutput
	if limit <= 0 {
		limit = maxOutput
	}
	over := overflowFail
	if c.MarkOverflow {
		over = overflowMark
	}
	return runCommand(ctx, timeout, limit, over, c.Env, c.Path, c.Args...)
}
