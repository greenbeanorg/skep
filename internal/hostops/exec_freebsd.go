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
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("hostops: refusing to run non-absolute command %q", path)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout, stderr capped
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		name := filepath.Base(path)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("hostops: %s timed out after %s", name, r.timeout)
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
	return stdout.buf.Bytes(), nil
}

// capped keeps at most maxOutput bytes and discards the rest.
type capped struct {
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
