//go:build unix

package hostops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The console log of a jail start and the capped jail -r output (ADR 0010
// amendment o, SR-01). These run real processes through /bin/sh: the pipe
// behaviour is POSIX, the same on the development machine and FreeBSD.

const testShell = "/bin/sh"

var testEnv = []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}

// shLines is what the shell loop of shLoop writes.
func shLines(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %06d of the jail's rc output, padded to be long enough\n", i)
	}
	return b.String()
}

// shLoop is a shell command that writes shLines(n).
func shLoop(n int) string {
	return fmt.Sprintf(`i=0; while [ $i -lt %d ]; do printf 'line %%06d of the jail'"'"'s rc output, padded to be long enough\n' $i; i=$((i+1)); done`, n)
}

func consoleCmd(t *testing.T, script string) (EnvCommand, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "console.log")
	return EnvCommand{Path: testShell, Args: []string{"-c", script}, Env: testEnv, ConsoleLog: log}, log
}

func TestConsoleLogLayout(t *testing.T) {
	const head, tail = 10, 6
	for name, tc := range map[string]struct {
		writes []string
		want   string
	}{
		"empty":             {nil, ""},
		"under the head":    {[]string{"abc", "def"}, "abcdef"},
		"exactly the head":  {[]string{"0123456789"}, "0123456789"},
		"head and a little": {[]string{"0123456789ab"}, "0123456789ab"},
		"head and the tail": {[]string{"0123456789", "abcdef"}, "0123456789abcdef"},
		"one byte dropped":  {[]string{"0123456789abcdefg"}, "0123456789" + discardedMarker(1) + "bcdefg"},
		"many small writes": {strings.Split("0123456789abcdefghijklmnopqrstuvwxyz", ""),
			"0123456789" + discardedMarker(20) + "uvwxyz"},
		"one large write": {[]string{strings.Repeat("x", 10) + strings.Repeat("y", 100) + "tail!!"},
			"xxxxxxxxxx" + discardedMarker(100) + "tail!!"},
		"large then small": {[]string{strings.Repeat("x", 50), "ab"},
			"xxxxxxxxxx" + discardedMarker(36) + "xxxxab"},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			l := newConsoleLog(&b, head, tail)
			for _, w := range tc.writes {
				if n, err := l.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if err := l.finish(); err != nil {
				t.Fatal(err)
			}
			if b.String() != tc.want {
				t.Fatalf("layout:\n got %q\nwant %q", b.String(), tc.want)
			}
		})
	}
}

// failingWriter fails every write after the first n bytes.
type failingWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.n {
		return 0, errors.New("no space left on device")
	}
	return w.buf.Write(p)
}

// A failed file write stops the writing and is reported, but the reading
// goes on: Write never fails, so the drain never stops and the command
// never blocks on a full pipe.
func TestConsoleLogWriteFailureKeepsDraining(t *testing.T) {
	w := &failingWriter{n: 4}
	l := newConsoleLog(w, 10, 4)
	for _, s := range []string{"abcd", "efgh", strings.Repeat("z", 100)} {
		if n, err := l.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if err := l.finish(); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("finish = %v, want the write error", err)
	}
	if w.buf.String() != "abcd" {
		t.Fatalf("written %q", w.buf.String())
	}
}

// The output of a start that goes past the head and the tail: the file
// holds the first ConsoleLogHead bytes, the marker with the exact count,
// and the last ConsoleLogTail bytes; it is never larger.
func TestRunToConsoleLogCapHitDuringStart(t *testing.T) {
	const n = 6000 // about 380 KiB: past head plus tail
	all := shLines(n)
	if len(all) <= ConsoleLogHead+ConsoleLogTail {
		t.Fatalf("the test output (%d bytes) does not pass the cap", len(all))
	}
	c, log := consoleCmd(t, shLoop(n))
	if err := runToConsoleLog(context.Background(), time.Minute, c); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := all[:ConsoleLogHead] + discardedMarker(int64(len(all)-ConsoleLogHead-ConsoleLogTail)) + all[len(all)-ConsoleLogTail:]
	if string(got) != want {
		t.Fatalf("console.log is %d bytes, want %d; starts %q, ends %q", len(got), len(want), got[:min(80, len(got))], got[max(0, len(got)-80):])
	}
	if !strings.HasSuffix(string(got), fmt.Sprintf("line %06d of the jail's rc output, padded to be long enough\n", n-1)) {
		t.Fatal("console.log does not end with rc's last line")
	}
	if fi, _ := os.Stat(log); fi.Mode().Perm() != 0o600 {
		t.Fatalf("console.log mode %04o, want 0600", fi.Mode().Perm())
	}
}

// Output under the cap is kept whole, and the file is truncated at each
// start: an earlier run's output is gone.
func TestRunToConsoleLogTruncatesAndKeepsShortOutput(t *testing.T) {
	c, log := consoleCmd(t, `echo "Starting cron."; echo "an error" >&2`)
	if err := os.WriteFile(log, []byte(strings.Repeat("old run\n", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runToConsoleLog(context.Background(), time.Minute, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "Starting cron.\nan error\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// A link at the console log's name is refused, not followed.
func TestRunToConsoleLogRefusesLink(t *testing.T) {
	c, log := consoleCmd(t, "echo hi")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, log); err != nil {
		t.Fatal(err)
	}
	if err := runToConsoleLog(context.Background(), time.Minute, c); err == nil {
		t.Fatal("a symbolic link at the console log was followed")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the link's target was written: %q", b)
	}
}

// A process the command leaves running with the pipe as its output does
// not hold the start: runToConsoleLog returns when the command exits,
// without waiting for end-of-file, and closes its end of the pipe.
func TestRunToConsoleLogPipeClosedAfterStart(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	c, log := consoleCmd(t, fmt.Sprintf(`echo started; sleep 30 & echo $! > %s`, pidFile))
	begin := time.Now()
	if err := runToConsoleLog(context.Background(), time.Minute, c); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Fatalf("runToConsoleLog waited %s for a process that kept the pipe", d)
	}
	t.Cleanup(func() { killPIDFile(pidFile) })
	got, _ := os.ReadFile(log)
	if string(got) != "started\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// After the start, a process left running that writes gets SIGPIPE (and
// dies), or EPIPE if it ignores the signal; the console log does not grow.
func TestRunToConsoleLogWriteAfterStart(t *testing.T) {
	for name, tc := range map[string]struct {
		trap string
		want string // what the background process records; "" = it died
	}{
		"default SIGPIPE kills the writer": {"", ""},
		"ignored SIGPIPE gives EPIPE":      {"trap '' PIPE; ", "write failed"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			res, ready := filepath.Join(dir, "result"), filepath.Join(dir, "ready")
			// The background writer waits until the start has ended
			// (the test creates ready), then writes to the pipe.
			bg := fmt.Sprintf(`(%swhile [ ! -e %s ]; do sleep 0.05; done; if echo late; then echo "write ok" > %s; else echo "write failed" > %s; fi; echo done >> %s.end) &`,
				tc.trap, ready, res, res, res)
			c, log := consoleCmd(t, "echo started; "+bg)
			if err := runToConsoleLog(context.Background(), time.Minute, c); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ready, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			end := res + ".end"
			deadline := time.Now().Add(5 * time.Second)
			if tc.want != "" {
				for time.Now().Before(deadline) {
					if _, err := os.Stat(end); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if b, _ := os.ReadFile(res); strings.TrimSpace(string(b)) != tc.want {
					t.Fatalf("the background writer recorded %q, want %q", b, tc.want)
				}
			} else {
				// The writer dies at its write: neither file appears.
				time.Sleep(1500 * time.Millisecond)
				for _, f := range []string{res, end} {
					if b, err := os.ReadFile(f); err == nil {
						t.Fatalf("the background writer survived its write to a closed pipe: %s = %q", filepath.Base(f), b)
					}
				}
			}
			if got, _ := os.ReadFile(log); string(got) != "started\n" {
				t.Fatalf("console.log grew after the start: %q", got)
			}
		})
	}
}

// A timed-out start ends too: the pipe is closed although a process
// still holds it, and what came before the timeout is in the log.
func TestRunToConsoleLogTimeout(t *testing.T) {
	c, log := consoleCmd(t, `echo before; sleep 30`)
	begin := time.Now()
	err := runToConsoleLog(context.Background(), 500*time.Millisecond, c)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Fatalf("the timeout took %s", d)
	}
	if got, _ := os.ReadFile(log); string(got) != "before\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// A failed command is a CommandError that points at the console log.
func TestRunToConsoleLogFailure(t *testing.T) {
	c, log := consoleCmd(t, `echo "rc: something broke"; exit 3`)
	err := runToConsoleLog(context.Background(), time.Minute, c)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.ExitCode != 3 || ce.Stderr != "output in console.log" {
		t.Fatalf("err = %#v", err)
	}
	if got, _ := os.ReadFile(log); string(got) != "rc: something broke\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// jail -r's output past the cap (the jail's /etc/rc.shutdown) keeps its
// head and its tail with a marker between them, and never makes the
// command fail; a real failure still does (amendments o and p).
func TestRunCommandMarkOverflow(t *testing.T) {
	const limit = 1000
	all := shLines(100)
	mark := runOpts{timeout: time.Minute, limit: limit, over: overflowMark, env: testEnv}
	out, err := runCommand(context.Background(), mark, testShell, "-c", shLoop(100))
	if err != nil {
		t.Fatalf("output past the cap failed the command: %v", err)
	}
	want := all[:limit] + discardedMarker(int64(len(all)-2*limit)) + all[len(all)-limit:]
	if string(out) != want {
		t.Fatalf("output:\n got %q\nwant %q", out, want)
	}
	// Under the cap: no marker.
	out, err = runCommand(context.Background(), mark, testShell, "-c", "echo short")
	if err != nil || string(out) != "short\n" {
		t.Fatalf("short output = %q, %v", out, err)
	}
	// Between the head and head plus tail: everything, no marker.
	out, err = runCommand(context.Background(), mark, testShell, "-c", shLoop(25))
	if err != nil || string(out) != shLines(25) {
		t.Fatalf("output under head plus tail = %q, %v", out, err)
	}
	// Run's mode is unchanged: cut silently, no marker.
	cut := mark
	cut.over = overflowCut
	out, err = runCommand(context.Background(), cut, testShell, "-c", shLoop(100))
	if err != nil || string(out) != all[:limit] {
		t.Fatalf("overflowCut = %d bytes, %v", len(out), err)
	}
	// The strict mode is unchanged: the same output is an error there.
	strict := mark
	strict.over = overflowFail
	if _, err := runCommand(context.Background(), strict, testShell, "-c", shLoop(100)); err == nil {
		t.Fatal("overflowFail accepted output past the cap")
	}
	// A failing command is still a failure in the marking mode.
	_, err = runCommand(context.Background(), mark, testShell, "-c", shLoop(100)+"; echo 'jail: no such jail' >&2; exit 1")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.ExitCode != 1 || ce.Stderr != "jail: no such jail" {
		t.Fatalf("err = %#v", err)
	}
}

// shutdownRunner is the test script with one real command: jail -r
// becomes a shell running sh, run by runCommand with the cap and the
// overflow mode the caller asked for, as the FreeBSD runner does.
type shutdownRunner struct {
	*script
	sh string
}

func (s shutdownRunner) RunEnv(ctx context.Context, c EnvCommand) ([]byte, error) {
	if c.Path != jailBinary || !slices.Contains(c.Args, "-r") {
		return s.script.RunEnv(ctx, c)
	}
	s.envCmds = append(s.envCmds, c)
	s.calls = append(s.calls, "jail "+strings.Join(c.Args, " "))
	if s.onRun != nil {
		s.onRun("jail " + strings.Join(c.Args, " "))
	}
	o := runOpts{timeout: time.Minute, limit: c.MaxOutput, over: overflowFail, env: testEnv}
	if o.limit <= 0 {
		o.limit = maxOutput
	}
	if c.MarkOverflow {
		o.over = overflowMark
	}
	return runCommand(ctx, o, testShell, "-c", s.sh)
}

// shutdownHost is a started jail whose jail -r runs sh for real, and the
// path of its console log, which holds rc's output.
func shutdownHost(t *testing.T, sh, rcOutput string) (*host, *script, string, string) {
	t.Helper()
	h, run, root := startedJail(t)
	h.run = shutdownRunner{script: run, sh: sh}
	conf, err := h.writeJailConfAt(jailSpec(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	log, err := h.conf.JailRunPath(ctrA, JailConsoleLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte(rcOutput), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, run, conf, log
}

// A stop whose /etc/rc.shutdown writes past the budget is a successful
// removal: jail -r is run in the marking mode with the shutdown budget,
// never the strict one, and the console log gets rc's output, the
// marker line, the first 32 KiB, a discarded marker, and the last 32 KiB
// (amendment p).
func TestStopJailShutdownOutputPastTheBudget(t *testing.T) {
	const n = 2000 // about 124 KiB
	h, run, conf, log := shutdownHost(t, shLoop(n), "rc output\n")
	name := "skep-" + ctrA
	if err := h.removeJail(context.Background(), name, conf, jailSpec(), true); err != nil {
		t.Fatalf("output past the budget failed the removal: %v", err)
	}
	var marked bool
	var limit int
	for _, c := range run.envCmds {
		if c.Path == jailBinary && slices.Contains(c.Args, "-r") {
			marked, limit = c.MarkOverflow, c.MaxOutput
		}
	}
	if !marked || limit != ShutdownLogPart || ShutdownLogPart != 32<<10 {
		t.Fatalf("jail -r: marking %v, cap %d", marked, limit)
	}
	if r := callsWith(run.calls, "jail "); !slices.Equal(r, []string{"jail -f " + conf + " -r " + name}) {
		t.Fatalf("removal: %q", r)
	}
	all := shLines(n)
	want := "rc output\n--- shutdown ---\n" + all[:ShutdownLogPart] +
		discardedMarker(int64(len(all)-2*ShutdownLogPart)) + all[len(all)-ShutdownLogPart:]
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("console.log: %d bytes, want %d\nhead %q\ntail %q", len(got), len(want), got[:min(len(got), 80)], got[max(0, len(got)-80):])
	}
}

// Under the budget the whole output is appended, with no discarded
// marker; the marker line starts on a line of its own even when rc's
// output did not end with a newline, and a shutdown that printed nothing
// still leaves the marker line.
func TestStopJailShutdownOutputUnderTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name, rc, sh, want string
	}{
		{"newline", "rc done\n", "echo 'Stopping cron.'; echo 'Terminated'",
			"rc done\n--- shutdown ---\nStopping cron.\nTerminated\n"},
		{"no newline", "rc done", "printf 'Stopping cron.\\n'",
			"rc done\n--- shutdown ---\nStopping cron.\n"},
		{"empty log", "", "echo bye", "--- shutdown ---\nbye\n"},
		{"silent", "rc done\n", "true", "rc done\n--- shutdown ---\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, conf, log := shutdownHost(t, tc.sh, tc.rc)
			if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, jailSpec(), true); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(log)
			if string(got) != tc.want {
				t.Fatalf("console.log = %q, want %q", got, tc.want)
			}
		})
	}
}

// A removal without exec.stop (a discard: none of the jail's code ran)
// appends nothing to the console log.
func TestDiscardAppendsNoShutdownOutput(t *testing.T) {
	h, _, conf, log := shutdownHost(t, "echo nothing", "rc output\n")
	if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, jailSpec(), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(log); string(got) != "rc output\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// A console log that cannot be written (a symbolic link, a directory, a
// FIFO, a missing runtime directory) never fails the removal: the jail
// is removed, the link's target and the FIFO are untouched, and the
// failure is logged.
func TestShutdownLogWriteFailureStillRemoves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, log string)
	}{
		{"symlink", func(t *testing.T, log string) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("host file\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Remove(log)
			if err := os.Symlink(target, log); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if b, _ := os.ReadFile(target); string(b) != "host file\n" {
					t.Errorf("link target = %q", b)
				}
			})
		}},
		{"directory", func(t *testing.T, log string) {
			os.Remove(log)
			if err := os.Mkdir(log, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, log string) {
			os.Remove(log)
			if err := mkfifo(log); err != nil {
				t.Fatal(err)
			}
		}},
		{"no runtime directory", func(t *testing.T, log string) {
			if err := os.RemoveAll(filepath.Dir(log)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, run, conf, log := shutdownHost(t, "echo 'Stopping cron.'", "rc output\n")
			tc.setup(t, log)
			var logged bytes.Buffer
			h.log = slog.New(slog.NewTextHandler(&logged, nil))
			name := "skep-" + ctrA
			done := make(chan error, 1)
			go func() { done <- h.removeJail(context.Background(), name, conf, jailSpec(), true) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("a failed console log write failed the removal: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the removal blocked on the console log")
			}
			if r := callsWith(run.calls, "jail "); !slices.Equal(r, []string{"jail -f " + conf + " -r " + name}) {
				t.Fatalf("removal: %q", r)
			}
			if !strings.Contains(logged.String(), "event=jail_shutdown_log_failed") {
				t.Fatalf("not logged: %s", logged.String())
			}
		})
	}
}

func killPIDFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err == nil && pid > 1 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
