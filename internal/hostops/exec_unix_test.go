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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
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
	// timeout is jail -r's; 0 is a minute.
	timeout time.Duration
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
	c.Env = testEnv
	timeout := s.timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	o := envRunOpts(timeout, c)
	out, err := runCommand(ctx, o, testShell, "-c", s.sh)
	var ce *CommandError
	if errors.As(err, &ce) {
		ce.Name = filepath.Base(jailBinary) // the shell stands in for jail(8)
	}
	return out, err
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
	if err := h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true); err != nil {
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
		discardedMarker(int64(len(all)-2*ShutdownLogPart)) + all[len(all)-ShutdownLogPart:] +
		"--- shutdown stderr ---\n"
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("console.log: %d bytes, want %d\nhead %q\ntail %q", len(got), len(want), got[:min(len(got), 80)], got[max(0, len(got)-80):])
	}
}

// rc.shutdown's stderr past the budget: its section is a discarded
// marker counting what was dropped and exactly the last 32 KiB, after
// the stdout section, itself cut to head and tail (amendment q).
func TestStopJailShutdownStderrPastTheBudget(t *testing.T) {
	const n = 2000 // about 124 KiB on each stream
	h, run, conf, log := shutdownHost(t, shLoop(n)+"; ( "+shLoop(n)+" ) >&2", "rc output\n")
	if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, testJailRoot, jailSpec(), true); err != nil {
		t.Fatalf("stderr past the budget failed the removal: %v", err)
	}
	var tail bool
	for _, c := range run.envCmds {
		if c.Path == jailBinary && slices.Contains(c.Args, "-r") {
			tail = c.StderrTail != nil && c.MaxOutput == ShutdownLogPart
		}
	}
	if !tail {
		t.Fatal("jail -r was not run with a stderr tail of ShutdownLogPart")
	}
	all := shLines(n)
	want := "rc output\n--- shutdown ---\n" + all[:ShutdownLogPart] +
		discardedMarker(int64(len(all)-2*ShutdownLogPart)) + all[len(all)-ShutdownLogPart:] +
		"--- shutdown stderr ---\n" +
		fmt.Sprintf("skep: %d bytes discarded\n", len(all)-ShutdownLogPart) + all[len(all)-ShutdownLogPart:]
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("console.log: %d bytes, want %d\ntail %q", len(got), len(want), got[max(0, len(got)-200):])
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
			"rc done\n--- shutdown ---\nStopping cron.\nTerminated\n--- shutdown stderr ---\n"},
		{"no newline", "rc done", "printf 'Stopping cron.\\n'",
			"rc done\n--- shutdown ---\nStopping cron.\n--- shutdown stderr ---\n"},
		{"empty log", "", "echo bye", "--- shutdown ---\nbye\n--- shutdown stderr ---\n"},
		{"silent", "rc done\n", "true", "rc done\n--- shutdown ---\n--- shutdown stderr ---\n"},
		{"stderr", "rc done\n", "echo 'Stopping cron.'; echo 'cron: not running' >&2",
			"rc done\n--- shutdown ---\nStopping cron.\n--- shutdown stderr ---\ncron: not running\n"},
		{"stdout without newline", "rc done\n", "printf 'Stopping'; printf 'oops' >&2",
			"rc done\n--- shutdown ---\nStopping\n--- shutdown stderr ---\noops"},
		{"only stderr", "rc done\n", "echo 'sshd: not running' >&2",
			"rc done\n--- shutdown ---\n--- shutdown stderr ---\nsshd: not running\n"},
		{"stderr kept raw", "rc done\n", `printf 'a\033[31m\342\200\256b\n' >&2`,
			"rc done\n--- shutdown ---\n--- shutdown stderr ---\na\x1b[31m\u202eb\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, conf, log := shutdownHost(t, tc.sh, tc.rc)
			if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, testJailRoot, jailSpec(), true); err != nil {
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
	if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, testJailRoot, jailSpec(), false); err != nil {
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
			h, run, conf, log := shutdownHost(t, "echo 'Stopping cron.'; echo 'cron: not running' >&2", "rc output\n")
			tc.setup(t, log)
			var logged bytes.Buffer
			h.log = slog.New(slog.NewTextHandler(&logged, nil))
			name := "skep-" + ctrA
			done := make(chan error, 1)
			go func() { done <- h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true) }()
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

// With stderrTo set, a successful run hands back the tail of stderr:
// all of it under the limit; past it, a discarded marker as the first
// line and exactly the last limit bytes. A failure writes nothing to it
// and still reports its stderr line; standard output is unchanged
// (amendment q).
func TestRunCommandStderrTail(t *testing.T) {
	const limit = 1000
	all := shLines(100)
	run := func(sh string, last bool) (string, string, error) {
		var errOut bytes.Buffer
		o := runOpts{timeout: time.Minute, limit: limit, over: overflowMark, env: testEnv, stderrLast: last, stderrTo: &errOut}
		out, err := runCommand(context.Background(), o, testShell, "-c", sh)
		return string(out), errOut.String(), err
	}
	for _, last := range []bool{false, true} {
		out, errOut, err := run("echo out; echo 'err 1' >&2; echo 'err 2' >&2", last)
		if err != nil || out != "out\n" || errOut != "err 1\nerr 2\n" {
			t.Fatalf("last %v, short: %q, %q, %v", last, out, errOut, err)
		}
		out, errOut, err = run("echo out; ( "+shLoop(100)+" ) >&2", last)
		want := fmt.Sprintf("skep: %d bytes discarded\n", len(all)-limit) + all[len(all)-limit:]
		if err != nil || out != "out\n" || errOut != want {
			t.Fatalf("last %v, long: %q, %d bytes, %v\nhead %q", last, out, len(errOut), err, errOut[:min(80, len(errOut))])
		}
		out, errOut, err = run("( "+shLoop(10)+" ) >&2", last)
		if err != nil || out != "" || errOut != shLines(10) {
			t.Fatalf("last %v, exactly under: %q, %v", last, errOut, err)
		}
		_, errOut, err = run("echo 'rc: a' >&2; echo 'jail: failed' >&2; exit 1", last)
		var ce *CommandError
		if !errors.As(err, &ce) || ce.ExitCode != 1 || errOut != "" {
			t.Fatalf("last %v, failure: %#v, stderr tail %q", last, err, errOut)
		}
		wantLine := "rc: a"
		if last {
			wantLine = "jail: failed"
		}
		if ce.Stderr != wantLine {
			t.Fatalf("last %v, failure line %q, want %q", last, ce.Stderr, wantLine)
		}
	}
	// Exactly the limit: no marker.
	exact := strings.Repeat("e", limit)
	_, errOut, err := run("printf '%s' "+exact+" >&2", false)
	if err != nil || errOut != exact {
		t.Fatalf("exactly the limit: %d bytes, %v", len(errOut), err)
	}
	// One more byte: the marker counts it.
	_, errOut, err = run("printf 'x%s' "+exact+" >&2", false)
	if err != nil || errOut != "skep: 1 bytes discarded\n"+exact {
		t.Fatalf("one byte over: %q, %v", errOut[:min(40, len(errOut))], err)
	}
	// A failing writer does not fail the command.
	o := runOpts{timeout: time.Minute, limit: limit, over: overflowMark, env: testEnv, stderrTo: &failingWriter{}}
	if out, err := runCommand(context.Background(), o, testShell, "-c", "echo out; echo err >&2"); err != nil || string(out) != "out\n" {
		t.Fatalf("failing stderr writer: %q, %v", out, err)
	}
}

// In the last-line mode a failure reports the last line of stderr,
// cleaned and capped, taken from the end of stderr even when it is
// longer than what is kept of it; an empty stderr reports nothing
// (amendment p).
func TestRunCommandStderrLastLine(t *testing.T) {
	o := runOpts{timeout: time.Minute, limit: 1000, over: overflowMark, env: testEnv, stderrLast: true}
	long := strings.Repeat("x", 500)
	for _, tc := range []struct{ name, sh, want string }{
		{"last line", `echo 'rc.shutdown: first' >&2; echo 'jail: skep-x: failed' >&2; exit 1`, "jail: skep-x: failed"},
		{"trailing blank lines", `printf 'rc: a\njail: b\n\n\n' >&2; exit 1`, "jail: b"},
		{"control characters", `printf 'rc: a\n\033[2J\033[31mjail:\tred\r\007\n' >&2; exit 1`, "[2J[31mjail:red"},
		{"over-long line", "printf 'rc: a\\n%s\\n' " + long + " >&2; exit 1", long[:200]},
		{"bidi override", `printf 'rc: a\n\342\200\256jail: \342\201\246x\342\201\251 ko\n' >&2; exit 1`, "jail: x ko"},
		{"past the kept tail", shLoop(2000) + " >&2; echo 'jail: the end' >&2; exit 1", "jail: the end"},
		{"empty stderr", `echo 'only stdout'; exit 1`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCommand(context.Background(), o, testShell, "-c", tc.sh)
			var ce *CommandError
			if !errors.As(err, &ce) || ce.ExitCode != 1 {
				t.Fatalf("err = %#v", err)
			}
			if ce.Stderr != tc.want {
				t.Fatalf("Stderr = %q, want %q", ce.Stderr, tc.want)
			}
		})
	}
	// The default is still the first line.
	o.stderrLast = false
	_, err := runCommand(context.Background(), o, testShell, "-c", `echo first >&2; echo last >&2; exit 1`)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Stderr != "first" {
		t.Fatalf("err = %#v", err)
	}
}

// forcedHost is shutdownHost whose jail -R is answered by jailR (nil
// for success) and makes the jail gone when it succeeds, with a devfs
// left on the jail's root/dev for the forced removal to unmount.
func forcedHost(t *testing.T, sh string, jailR error) (*host, *script, *fakeMounts, string, string) {
	t.Helper()
	h, run, conf, log := shutdownHost(t, sh, "rc output\n")
	name := "skep-" + ctrA
	if jailR != nil {
		run.answers["jail -R "+name] = jailR
	}
	m := &fakeMounts{run: run, table: []fsMount{
		{On: "/dev", FSType: "devfs", FSID: [2]int32{1, 1}},
		{On: testJailRoot + "/dev", FSType: "devfs", FSID: [2]int32{7, 9}},
	}}
	h.mounts = m
	// RACCT on, with one rule on the jail until rctl -r removes it.
	sys := h.sys.(fakeSystem)
	sys.oids[sysctlRacct], sys.oids[sysctlNCPU] = 1, 4
	run.answers["rctl jail:"+name] = []any{"jail:" + name + ":maxproc:deny=100\n", ""}
	// jail -r fails, so the jail stays until jail -R succeeds.
	present := "12\n"
	gone := &CommandError{Name: "jls", ExitCode: 1, Stderr: "jls: jail \"" + name + "\" not found", Err: errors.New("exit status 1")}
	run.answers["jls -j "+name+" jid"] = present
	run.onRun = func(call string) {
		if call == "jail -R "+name && jailR == nil {
			run.answers["jls -j "+name+" jid"] = gone
		}
	}
	return h, run, m, conf, log
}

// A failed jail -r is no longer the end of a stop (amendment u): the
// console log gets rc.shutdown's stdout and stderr sections as for a
// successful stop, then "--- jail -r failed: <line> ---" with the last
// stderr line cleaned (control and format characters removed, at most
// 200 bytes); the jail is removed with jail -R, after jail -r; the
// leftover devfs is unmounted by fsid and the RCTL rules are removed; the
// stop succeeds, and the log has jail_remove_failed and
// container_stop_forced with the cleaned line and no control characters.
func TestRemoveJailFailureForcesRemoval(t *testing.T) {
	for _, tc := range []struct{ name, sh, wantLine, wantSections string }{
		{"control characters",
			`echo 'Stopping cron.'; echo 'rc.shutdown: jail: everything is fine' >&2; printf 'jail: skep-x: \033[31mfailed\r\n' >&2; exit 1`,
			"jail: skep-x: [31mfailed",
			"--- shutdown ---\nStopping cron.\n--- shutdown stderr ---\nrc.shutdown: jail: everything is fine\njail: skep-x: \x1b[31mfailed\r\n"},
		{"over-long line",
			"printf '%s\\n' " + strings.Repeat("y", 400) + " >&2; exit 1",
			strings.Repeat("y", 200),
			"--- shutdown ---\n--- shutdown stderr ---\n" + strings.Repeat("y", 400) + "\n"},
		{"bidi override",
			`printf 'jail: skep-x: \342\200\256deliaf\342\200\254 ok' >&2; exit 1`,
			"jail: skep-x: deliaf ok",
			"--- shutdown ---\n--- shutdown stderr ---\njail: skep-x: \u202edeliaf\u202c ok\n"},
		{"empty stderr",
			`echo 'Stopping cron.'; exit 1`,
			"no jail output on stderr",
			"--- shutdown ---\nStopping cron.\n--- shutdown stderr ---\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, run, m, conf, log := forcedHost(t, tc.sh, nil)
			var logged bytes.Buffer
			h.log = slog.New(slog.NewTextHandler(&logged, nil))
			name := "skep-" + ctrA
			if err := h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true); err != nil {
				t.Fatalf("a failed jail -r failed the removal: %v", err)
			}
			want := "rc output\n" + tc.wantSections + "--- jail -r failed: " + tc.wantLine + " ---\n"
			if got, _ := os.ReadFile(log); string(got) != want {
				t.Fatalf("console.log:\n got %q\nwant %q", got, want)
			}
			r := slices.Index(run.calls, "jail -f "+conf+" -r "+name)
			force := slices.Index(run.calls, "jail -R "+name)
			unmount := slices.Index(run.calls, "unmount FSID:7:9")
			rctl := slices.Index(run.calls, "rctl -r jail:"+name)
			if r < 0 || force < r || unmount < force || rctl < force {
				t.Fatalf("jail -r at %d, jail -R at %d, unmount at %d, rctl at %d: %q", r, force, unmount, rctl, run.calls)
			}
			if !slices.Equal(m.unmounted, [][2]int32{{7, 9}}) {
				t.Fatalf("unmounted %v, want only the jail's devfs", m.unmounted)
			}
			for _, c := range run.envCmds {
				if c.Path == jailBinary && slices.Contains(c.Args, "-R") {
					if !slices.Equal(c.Env, jailEnv) || c.Timeout != jailCommandSlack || !c.StderrLastLine {
						t.Fatalf("jail -R: %+v", c)
					}
				}
				if c.Path == jailBinary && slices.Contains(c.Args, "-r") && !c.OutputOnFailure {
					t.Fatal("jail -r was not run with OutputOnFailure")
				}
			}
			l := logged.String()
			if !strings.Contains(l, "event=jail_remove_failed") || !strings.Contains(l, "event=container_stop_forced") {
				t.Fatalf("log: %s", l)
			}
			if !strings.Contains(l, "jail_output="+strconv.Quote(tc.wantLine)) && !strings.Contains(l, "jail_output="+tc.wantLine+"\n") &&
				!strings.Contains(l, "jail_output="+tc.wantLine+" ") {
				t.Fatalf("log has not the cleaned line %q: %s", tc.wantLine, l)
			}
			if strings.ContainsAny(l, "\x1b\r\u202e\u202c") {
				t.Fatalf("control characters in the log: %q", l)
			}
		})
	}
}

// When jail -R fails too, the removal fails with container_remove_failed,
// whose message has both cleaned stderr lines; the console log still has
// the sections and the failure line, and nothing is unmounted (amendment
// u).
func TestRemoveJailForcedRemovalFails(t *testing.T) {
	jailR := &CommandError{Name: "jail", ExitCode: 1, Stderr: "jail: skep-x: \u202ebusy", Err: errors.New("exit status 1")}
	// The runner cleans the line; the fake gives it raw, so it is cleaned
	// as the runner would.
	jailR.Stderr = cleanLine(jailR.Stderr, maxStderrLine)
	h, run, m, conf, log := forcedHost(t, `echo 'r2pb rc.shutdown exiting 1' >&2; printf 'jail: skep-x: /bin/sh /etc/rc.shutdown jail: \033failed\n' >&2; exit 1`, jailR)
	var logged bytes.Buffer
	h.log = slog.New(slog.NewTextHandler(&logged, nil))
	name := "skep-" + ctrA
	err := h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true)
	wantCode(t, err, models.CodeContainerRemoveFailed)
	msg := err.Error()
	for _, want := range []string{`jail: skep-x: /bin/sh /etc/rc.shutdown jail: failed`, `jail: skep-x: busy`, "jail -R"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
	if strings.ContainsAny(msg, "\x1b\u202e") || strings.ContainsAny(logged.String(), "\x1b\u202e") {
		t.Fatalf("control characters: %q", msg)
	}
	if !strings.Contains(logged.String(), "event=container_remove_failed") || strings.Contains(logged.String(), "container_stop_forced") {
		t.Fatalf("log: %s", logged.String())
	}
	if len(m.unmounted) != 0 || slices.Contains(run.calls, "rctl -r jail:"+name) {
		t.Fatalf("cleanup ran after a failed jail -R: %v %q", m.unmounted, run.calls)
	}
	want := "rc output\n--- shutdown ---\n--- shutdown stderr ---\nr2pb rc.shutdown exiting 1\njail: skep-x: /bin/sh /etc/rc.shutdown jail: \x1bfailed\n" +
		"--- jail -r failed: jail: skep-x: /bin/sh /etc/rc.shutdown jail: failed ---\n"
	if got, _ := os.ReadFile(log); string(got) != want {
		t.Fatalf("console.log:\n got %q\nwant %q", got, want)
	}
	// A jail -R that exits 0 but leaves the jail is the same failure.
	h, _, _, conf, _ = forcedHost(t, "exit 1", nil)
	h.run.(shutdownRunner).onRun = nil
	err = h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true)
	wantCode(t, err, models.CodeContainerRemoveFailed)
	if !strings.Contains(err.Error(), "still there after jail -R") {
		t.Fatalf("err = %v", err)
	}
}

// A jail -r that outlasts its timeout (rc.shutdown hanging) is a failure
// under amendment u: what it wrote before is appended, the failure line
// says it timed out and gives its last stderr line, and jail -R removes
// the jail.
func TestRemoveJailTimeoutForcesRemoval(t *testing.T) {
	h, run, _, conf, log := forcedHost(t, `echo 'Stopping sshd.'; echo 'sshd: waiting' >&2; exec sleep 30`, nil)
	sr := h.run.(shutdownRunner)
	sr.timeout = 300 * time.Millisecond
	h.run = sr
	name := "skep-" + ctrA
	start := time.Now()
	if err := h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true); err != nil {
		t.Fatalf("a timed-out jail -r failed the removal: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
	want := "rc output\n--- shutdown ---\nStopping sshd.\n--- shutdown stderr ---\nsshd: waiting\n" +
		"--- jail -r failed: timed out after 300ms, last line of stderr: sshd: waiting ---\n"
	if got, _ := os.ReadFile(log); string(got) != want {
		t.Fatalf("console.log:\n got %q\nwant %q", got, want)
	}
	if !slices.Contains(run.calls, "jail -R "+name) {
		t.Fatalf("no jail -R: %q", run.calls)
	}
}

// A removal the caller cancels is not forced: no jail -R, nothing
// appended.
func TestRemoveJailCancelledIsNotForced(t *testing.T) {
	h, run, _, conf, log := forcedHost(t, `exec sleep 30`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	run.onRun = func(call string) {
		if strings.Contains(call, " -r ") {
			go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		}
	}
	err := h.removeJail(ctx, "skep-"+ctrA, conf, testJailRoot, jailSpec(), true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if slices.ContainsFunc(run.calls, func(c string) bool { return strings.HasPrefix(c, "jail -R") }) {
		t.Fatalf("a cancelled removal was forced: %q", run.calls)
	}
	if got, _ := os.ReadFile(log); string(got) != "rc output\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// A discard (no exec.stop: none of the jail's code may run) whose jail -r
// fails is forced the same way, and appends nothing to the console log.
func TestDiscardFailureForcesRemovalWithoutLog(t *testing.T) {
	h, run, _, conf, log := forcedHost(t, `echo 'jail: failed' >&2; exit 1`, nil)
	if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, testJailRoot, jailSpec(), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(run.calls, "jail -R skep-"+ctrA) {
		t.Fatalf("no jail -R: %q", run.calls)
	}
	if got, _ := os.ReadFile(log); string(got) != "rc output\n" {
		t.Fatalf("console.log = %q", got)
	}
}

// jail -r exits 0 but the jail is still there: the sections and a failure
// line are appended, and jail -R removes it.
func TestRemoveJailLeftJailForcesRemoval(t *testing.T) {
	h, run, _, conf, log := forcedHost(t, `echo bye`, nil)
	if err := h.removeJail(context.Background(), "skep-"+ctrA, conf, testJailRoot, jailSpec(), true); err != nil {
		t.Fatal(err)
	}
	want := "rc output\n--- shutdown ---\nbye\n--- shutdown stderr ---\n--- jail -r failed: the jail was still there after jail -r exited 0 ---\n"
	if got, _ := os.ReadFile(log); string(got) != want {
		t.Fatalf("console.log = %q", got)
	}
	if !slices.Contains(run.calls, "jail -R skep-"+ctrA) {
		t.Fatalf("no jail -R: %q", run.calls)
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

// TestCleanLineFirstStderrLine: a failed command run without the
// last-line mode reports its first stderr line; cleanStderr, as jail -c
// and a build jail's command use it (amendment r), removes the control
// and format characters the shell wrote, on real pipes.
func TestCleanLineFirstStderrLine(t *testing.T) {
	o := runOpts{env: testEnv, timeout: 10 * time.Second, limit: maxOutput}
	sh := `printf '\033[31mfake \342\200\256ok\342\200\254\302\233x\r\nsecond line\n' >&2; exit 3`
	_, err := runCommand(context.Background(), o, testShell, "-c", sh)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.ExitCode != 3 || !strings.Contains(ce.Stderr, "\u202e") {
		t.Fatalf("runCommand = %v (stderr %q)", err, ce.Stderr)
	}
	err = cleanStderr(err)
	if ce.Stderr != "[31mfake okx" {
		t.Fatalf("cleaned first line = %q", ce.Stderr)
	}
	for _, r := range err.Error() {
		if unsafeRune(r) {
			t.Fatalf("the error kept %U: %q", r, err.Error())
		}
	}
}

// TestRunEnvStdin: a command gets EnvCommand.Stdin as its whole standard
// input (ngctl -f -, ADR 0011), and none without it.
func TestRunEnvStdin(t *testing.T) {
	o := envRunOpts(5*time.Second, EnvCommand{Stdin: []byte("mkpeer x\nconnect y\n")})
	out, err := runCommand(context.Background(), o, testShell, "-c", "cat")
	if err != nil || string(out) != "mkpeer x\nconnect y\n" {
		t.Fatalf("stdin: %q, %v", out, err)
	}
	out, err = runCommand(context.Background(), envRunOpts(5*time.Second, EnvCommand{}), testShell, "-c", "cat")
	if err != nil || len(out) != 0 {
		t.Fatalf("no stdin: %q, %v", out, err)
	}
}
