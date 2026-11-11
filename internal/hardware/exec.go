package hardware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// searchDirs is the fixed list of directories searched for executables.
// Discovery deliberately ignores $PATH: the agent may run with privileges,
// and its environment must not decide which binary gets executed.
var searchDirs = []string{
	"/sbin", "/bin", "/usr/sbin", "/usr/bin", "/usr/local/sbin", "/usr/local/bin",
}

// maxCommandOutput caps how much output is kept from one command.
const maxCommandOutput = 1 << 20

// findInDirs returns a pathFinder that searches dirs for an executable file.
func findInDirs(dirs []string) pathFinder {
	return func(name string) (string, bool) {
		if name == "" || strings.ContainsRune(name, filepath.Separator) {
			return "", false
		}
		for _, dir := range dirs {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				return p, true
			}
		}
		return "", false
	}
}

// execRunner is the real CommandRunner. It runs an absolute path with an
// explicit argument list, a minimal fixed environment, no stdin, a timeout,
// and capped output. No shell is involved.
type execRunner struct {
	timeout time.Duration
}

func (r execRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("refusing to run non-absolute command %q", path)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout, stderr cappedBuffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", filepath.Base(path), r.timeout)
		}
		if msg := firstLine(stderr.Bytes()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", filepath.Base(path), err, msg)
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return stdout.Bytes(), nil
}

// cappedBuffer keeps at most maxCommandOutput bytes and discards the rest.
type cappedBuffer struct {
	buf bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxCommandOutput - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(line)
}
