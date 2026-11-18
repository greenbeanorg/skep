//go:build unix

package hostops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The command runner's portable part: plain POSIX process and pipe
// handling, built on every Unix so its tests run on the development
// machine as well as on FreeBSD. The Runner that uses it is FreeBSD's
// (exec_freebsd.go).

// maxOutput caps what is kept of one command's output.
const maxOutput = 4 << 20

// overflow is what runCommand does with output past its limit.
type overflow int

const (
	// overflowCut keeps the first limit bytes and drops the rest silently.
	overflowCut overflow = iota
	// overflowFail makes output past the limit an error (the template
	// builder parses pkg's whole output).
	overflowFail
	// overflowMark keeps the first limit bytes and the last limit bytes,
	// with a marker between them that counts the bytes dropped; it is
	// never an error (ADR 0010 amendments o and p: jail -r, whose output
	// is the jail's /etc/rc.shutdown's). At most 2*limit bytes are held.
	overflowMark
)

// runOpts is how runCommand runs one command.
type runOpts struct {
	timeout time.Duration
	// limit caps what is kept of standard output; over says what happens
	// past it.
	limit int
	over  overflow
	// env is the whole environment.
	env []string
}

// runCommand runs one command with an explicit argument list, the given
// whole environment, no stdin, a timeout, and capped output.
func runCommand(ctx context.Context, o runOpts, path string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("hostops: refusing to run non-absolute command %q", path)
	}
	timeout := o.timeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr := capped{limit: o.limit}, capped{limit: maxOutput}
	// The marking mode keeps the head and the tail of standard output in
	// memory, laid out as the console log is: the head, then (if more
	// came) the marker and the tail.
	var marked bytes.Buffer
	headTail := newConsoleLog(&marked, o.limit, o.limit)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = o.env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if o.over == overflowMark {
		cmd.Stdout = headTail
	}
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
	if o.over == overflowMark {
		// Writes to a bytes.Buffer do not fail.
		_ = headTail.finish()
		return marked.Bytes(), nil
	}
	if stdout.over && o.over == overflowFail {
		return nil, fmt.Errorf("hostops: %s wrote more than %d bytes", filepath.Base(path), o.limit)
	}
	return stdout.buf.Bytes(), nil
}

// discardedMarker is the line written where output was dropped.
func discardedMarker(n int64) string {
	return fmt.Sprintf("\nskep: %d bytes discarded\n", n)
}

// capped keeps at most limit bytes, discards the rest, and remembers
// that it did and how much it saw.
type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
	total int64
}

func (c *capped) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	room := c.limit - c.buf.Len()
	if room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	if len(p) > room {
		c.over = true
	}
	return len(p), nil
}

// The console log of a jail start (ADR 0010 amendment o, SR-01).
const (
	// ConsoleLogHead is how much of a start's output is written as it
	// arrives.
	ConsoleLogHead = 256 << 10
	// ConsoleLogTail is how much of the end of the output is kept, and
	// written after the marker, when the output is longer than the head.
	ConsoleLogTail = 64 << 10
	// consoleDrainGrace bounds how long the pipe is still read after the
	// command exited: what it wrote before exiting is in the pipe buffer.
	// End-of-file is never waited for, since processes the command left
	// running may hold the pipe forever.
	consoleDrainGrace = 100 * time.Millisecond
)

// runToConsoleLog runs a command whose standard output and error go to a
// pipe that the agent drains into c.ConsoleLog: the file is truncated, the
// first ConsoleLogHead bytes are written as they arrive, the rest is
// counted and dropped except the last ConsoleLogTail bytes, which are
// written after a marker when the command has ended. The read end is
// closed as soon as the command has ended (exited, failed, or timed out),
// so a process the command left running that writes later gets SIGPIPE,
// or EPIPE if it ignores the signal; no process ever holds a descriptor
// on the host file.
func runToConsoleLog(ctx context.Context, timeout time.Duration, c EnvCommand) error {
	if !filepath.IsAbs(c.Path) || !filepath.IsAbs(c.ConsoleLog) {
		return fmt.Errorf("hostops: refusing a non-absolute command %q or console log %q", c.Path, c.ConsoleLog)
	}
	f, err := os.OpenFile(c.ConsoleLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("hostops: open %s: %w", c.ConsoleLog, err)
	}
	defer f.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("hostops: pipe for %s: %w", filepath.Base(c.Path), err)
	}
	defer r.Close()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	// An *os.File, not an io.Writer: the child gets the pipe itself and
	// Wait returns when the command exits, not at end-of-file.
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close() // only the child and what it starts hold the write end
	if err != nil {
		return fmt.Errorf("hostops: start %s: %w", filepath.Base(c.Path), err)
	}
	log := newConsoleLog(f, ConsoleLogHead, ConsoleLogTail)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainPipe(r, log)
	}()
	runErr := cmd.Wait()
	// The command has ended: read what it left in the pipe for a short,
	// bounded time, then close the read end whatever still holds the
	// write end.
	if err := r.SetReadDeadline(time.Now().Add(consoleDrainGrace)); err != nil {
		r.Close()
	}
	<-drained
	r.Close()
	logErr := log.finish()

	if runErr != nil {
		name := filepath.Base(c.Path)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("hostops: %s timed out after %s", name, timeout)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ce := &CommandError{Name: name, ExitCode: -1, Err: runErr, Stderr: "output in " + filepath.Base(c.ConsoleLog)}
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			ce.ExitCode = exit.ExitCode()
		}
		return ce
	}
	if logErr != nil {
		return fmt.Errorf("hostops: writing %s: %w", c.ConsoleLog, logErr)
	}
	return nil
}

// drainPipe copies r into log until r fails: end-of-file, its deadline,
// or its closing.
func drainPipe(r io.Reader, log *consoleLog) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			log.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// consoleLog lays a command's output out in a file: the first head bytes
// as they arrive; at finish, when more came, a marker counting the bytes
// dropped and then the last tail bytes. A failed write to the file stops
// the writing, never the reading, so the command never blocks on the
// pipe.
type consoleLog struct {
	w       io.Writer
	head    int
	tailMax int
	written int
	tail    []byte
	over    int64
	err     error
}

func newConsoleLog(w io.Writer, head, tail int) *consoleLog {
	return &consoleLog{w: w, head: head, tailMax: tail}
}

func (l *consoleLog) Write(p []byte) (int, error) {
	n := len(p)
	if room := l.head - l.written; room > 0 {
		k := min(room, len(p))
		if l.err == nil {
			_, l.err = l.w.Write(p[:k])
		}
		l.written += k
		p = p[k:]
	}
	if len(p) > 0 {
		l.over += int64(len(p))
		if len(p) >= l.tailMax {
			l.tail = append(l.tail[:0], p[len(p)-l.tailMax:]...)
		} else {
			l.tail = append(l.tail, p...)
			if extra := len(l.tail) - l.tailMax; extra > 0 {
				l.tail = append(l.tail[:0], l.tail[extra:]...)
			}
		}
	}
	return n, nil
}

// finish writes the marker and the tail, if output went past the head,
// and returns the first write error.
func (l *consoleLog) finish() error {
	if l.over == 0 || l.err != nil {
		return l.err
	}
	if dropped := l.over - int64(len(l.tail)); dropped > 0 {
		if _, l.err = io.WriteString(l.w, discardedMarker(dropped)); l.err != nil {
			return l.err
		}
	}
	_, l.err = l.w.Write(l.tail)
	return l.err
}
