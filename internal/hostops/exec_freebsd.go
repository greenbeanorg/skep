//go:build freebsd

package hostops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxOutput caps what is kept of one command's output.
const maxOutput = 4 << 20

// execRunner is the real Runner: an absolute path, an explicit argument
// list, a fixed minimal environment, no stdin, a timeout, capped output.
type execRunner struct {
	timeout time.Duration
}

func (r execRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return runCommand(ctx, r.timeout, maxOutput, false,
		[]string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}, path, args...)
}

// runCommand runs one command. strict makes output past the limit an
// error instead of silently cut (the builder parses pkg's whole output).
func runCommand(ctx context.Context, timeout time.Duration, limit int, strict bool, env []string, path string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("hostops: refusing to run non-absolute command %q", path)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr := capped{limit: limit}, capped{limit: maxOutput}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		name := filepath.Base(path)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("hostops: %s timed out after %s", name, timeout)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ce := &CommandError{Name: name, ExitCode: -1, Err: err}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			ce.ExitCode = exit.ExitCode()
		}
		line, _, _ := strings.Cut(strings.TrimSpace(stderr.buf.String()), "\n")
		ce.Stderr = strings.TrimSpace(line)
		return nil, ce
	}
	if strict && stdout.over {
		return nil, fmt.Errorf("hostops: %s wrote more than %d bytes", filepath.Base(path), limit)
	}
	return stdout.buf.Bytes(), nil
}

// RunEnv implements EnvRunner: the same rules as Run, with the command's
// own complete environment, timeout, and output cap.
func (r execRunner) RunEnv(ctx context.Context, c EnvCommand) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	limit := c.MaxOutput
	if limit <= 0 {
		limit = maxOutput
	}
	return runCommand(ctx, timeout, limit, true, c.Env, c.Path, c.Args...)
}

// capped keeps at most limit bytes, discards the rest, and remembers
// that it did.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	if len(p) > room {
		c.over = true
	}
	return len(p), nil
}
