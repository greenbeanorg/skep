// Command adminprompt-test is the native integration test for the password
// prompt of "skep-api admin create" and "skep-api admin reset-password"
// (ADR 0006 decision 8). It runs the real skep-api binary on a
// pseudo-terminal, types the password the way a person would, and checks
// that the terminal never shows it, that echo is switched off while it is
// typed and back on afterwards, and that the account works.
//
// It also measures the Argon2id hash time with "admin hash-benchmark".
//
// It runs as an ordinary user in a scratch directory and changes nothing
// else.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var failed int

func check(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("ok     %s\n", msg)
	} else {
		failed++
		fmt.Printf("FAIL   %s\n", msg)
	}
	return ok
}

func step(format string, args ...any) { fmt.Printf("\n== %s\n", fmt.Sprintf(format, args...)) }

func fatal(format string, args ...any) {
	fmt.Printf("ABORT  %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

// screen collects what the terminal shows. A goroutine fills it: read
// deadlines do not work on a pseudo-terminal master.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) pump(master *os.File) {
	buf := make([]byte, 1024)
	for {
		n, err := master.Read(buf)
		s.mu.Lock()
		s.buf.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// session is one command running on a pseudo-terminal.
type session struct {
	master, slave *os.File
	cmd           *exec.Cmd
	shown         screen
	done          chan error
}

// wrapper runs the command under a shell that stays alive for a few
// seconds afterwards. The shell is the session leader, so the terminal
// outlives the command and its settings can still be read: when a session
// leader exits, FreeBSD revokes its terminal. "trap :" lets the shell
// survive a Ctrl-C that is meant for the command.
const wrapper = `trap : INT; "$0" "$@"; echo "skep-exit=$?"; sleep 5`

// startOnTerminal runs the command with a new pseudo-terminal as its
// controlling terminal, standard input, output, and error.
func startOnTerminal(bin string, args ...string) *session {
	master, slave, err := openPTY()
	if err != nil {
		fatal("open a pseudo-terminal: %v", err)
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", wrapper, bin}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		fatal("start %s: %v", bin, err)
	}
	s := &session{master: master, slave: slave, cmd: cmd, done: make(chan error, 1)}
	go s.shown.pump(master)
	go func() { s.done <- cmd.Wait() }()
	return s
}

// waitFor waits until the terminal has shown want.
func (s *session) waitFor(want string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(s.shown.String(), want) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func (s *session) echo() bool {
	tio, err := unix.IoctlGetTermios(int(s.slave.Fd()), ioctlGetTermios)
	if err != nil {
		fatal("read the terminal settings: %v", err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func (s *session) typeLine(line string) {
	if _, err := s.master.WriteString(line + "\n"); err != nil {
		fatal("type on the terminal: %v", err)
	}
}

var exitRE = regexp.MustCompile(`skep-exit=(\d+)`)

// finish waits for the command to exit and returns its exit status. The
// terminal stays usable for a few seconds after that.
func (s *session) finish() (exit int) {
	if !s.waitFor("skep-exit=") {
		fatal("the command did not exit; the terminal showed %q", s.shown.String())
	}
	time.Sleep(200 * time.Millisecond)
	m := exitRE.FindStringSubmatch(s.shown.String())
	if m == nil {
		fatal("no exit status on the terminal: %q", s.shown.String())
	}
	exit, _ = strconv.Atoi(m[1])
	return exit
}

// close ends the shell and the terminal.
func (s *session) close() {
	s.cmd.Process.Kill()
	<-s.done
	s.master.Close()
	s.slave.Close()
}

func runCmd(stdin string, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func main() {
	bin := flag.String("bin", "./skep-api", "the skep-api binary to test")
	flag.Parse()
	if os.Geteuid() == 0 {
		fatal("run this test as an ordinary user")
	}
	fmt.Printf("       %s/%s, uid %d\n", runtime.GOOS, runtime.GOARCH, os.Getuid())
	if out, err := runCmd("", *bin, "-version"); err != nil {
		fatal("%s -version: %v", *bin, err)
	} else {
		fmt.Printf("       %s", out)
	}
	scratch, err := os.MkdirTemp("", "skep-adminprompt")
	if err != nil {
		fatal("%v", err)
	}
	defer os.RemoveAll(scratch)
	dataDir := filepath.Join(scratch, "data")
	common := []string{"-dev", "-data-dir", dataDir}

	const password = "Tr0ub4dor&3-horse-staple"
	const second = "a-different-passphrase-99"

	step("admin create asks for the password on the terminal, without echo")
	s := startOnTerminal(*bin, append([]string{"admin", "create", "--username", "andy"}, common...)...)
	check(s.echo(), "the terminal echoes before the prompt")
	check(s.waitFor("New password for andy: "), "the first prompt appears")
	// Give the program a moment to have switched echo off: the prompt is
	// printed after that, so it already is.
	check(!s.echo(), "echo is off while the password is typed")
	s.typeLine(password)
	check(s.waitFor("Repeat the password: "), "the second prompt appears")
	check(!s.echo(), "echo is off for the repeat")
	s.typeLine(password)
	exit := s.finish()
	check(exit == 0, "the command succeeds (exit %d)", exit)
	check(strings.Contains(s.shown.String(), `Administrator "andy" created`), "it reports the new administrator")
	check(!strings.Contains(s.shown.String(), password) && !strings.Contains(s.shown.String(), "Tr0ub4dor"),
		"the terminal never showed the password")
	check(s.echo(), "echo is back on afterwards")
	fmt.Printf("       the terminal showed: %q\n", strings.Split(s.shown.String(), "skep-exit=")[0])
	s.close()

	step("the account exists and the password is the one typed")
	out, err := runCmd("", *bin, append([]string{"admin", "create", "--username", "andy", "--password-stdin"}, common...)...)
	check(err != nil && strings.Contains(out, "already exists"), "a second create of the same name is refused")

	step("passwords that differ change nothing")
	s = startOnTerminal(*bin, append([]string{"admin", "create", "--username", "bob"}, common...)...)
	s.waitFor("New password for bob: ")
	s.typeLine(password)
	s.waitFor("Repeat the password: ")
	s.typeLine(password + "x")
	exit = s.finish()
	check(exit != 0 && strings.Contains(s.shown.String(), "not the same"), "refused (exit %d)", exit)
	check(!strings.Contains(s.shown.String(), "Tr0ub4dor"), "the terminal never showed either password")
	check(s.echo(), "echo is back on after the refusal")
	s.close()

	step("Ctrl-C at the prompt restores the terminal")
	s = startOnTerminal(*bin, append([]string{"admin", "create", "--username", "carol"}, common...)...)
	s.waitFor("New password for carol: ")
	check(!s.echo(), "echo is off at the prompt")
	if _, err := s.master.Write([]byte{0x03}); err != nil { // the terminal turns this into SIGINT
		fatal("type Ctrl-C: %v", err)
	}
	exit = s.finish()
	check(exit != 0, "the command stops (exit %d)", exit)
	check(s.echo(), "echo is back on after Ctrl-C")
	s.close()

	step("reset-password on the terminal")
	s = startOnTerminal(*bin, append([]string{"admin", "reset-password", "--username", "andy"}, common...)...)
	check(s.waitFor("New password for andy: "), "the prompt appears")
	s.typeLine(second)
	s.waitFor("Repeat the password: ")
	s.typeLine(second)
	exit = s.finish()
	check(exit == 0 && strings.Contains(s.shown.String(), "was replaced"), "the password is replaced (exit %d)", exit)
	check(!strings.Contains(s.shown.String(), second), "the terminal never showed the new password")
	s.close()

	step("without a terminal the command refuses instead of reading a pipe")
	cmd := exec.Command(*bin, append([]string{"admin", "create", "--username", "dave"}, common...)...)
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // no controlling terminal
	raw, err := cmd.CombinedOutput()
	check(err != nil && strings.Contains(string(raw), "--password-stdin"), "refused, and it names --password-stdin: %q", strings.TrimSpace(string(raw)))

	step("--password-stdin for unattended installs")
	out, err = runCmd(password+"\n", *bin, append([]string{"admin", "create", "--username", "erin", "--password-stdin"}, common...)...)
	check(err == nil && strings.Contains(out, `Administrator "erin" created`), "created from standard input (%v)", err)
	out, err = runCmd("", *bin, append([]string{"admin", "create", "--username", "frank", "--password", password}, common...)...)
	check(err != nil && strings.Contains(out, "not defined"), "a password as an argument is not accepted")

	step("Argon2id timing on this host (default parameters)")
	out, err = runCmd("", *bin, append([]string{"admin", "hash-benchmark", "--rounds", "10"}, common...)...)
	check(err == nil, "hash-benchmark ran (%v)", err)
	fmt.Print("       ", strings.ReplaceAll(strings.TrimSpace(out), "\n", "\n       "), "\n")
	check(strings.Contains(out, "Within the 100 to 500 ms target"), "one hash takes 100 to 500 ms")

	if failed > 0 {
		fmt.Printf("\nFAILED: %d check(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nPASS")
}
