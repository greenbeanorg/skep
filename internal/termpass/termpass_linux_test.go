package termpass

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY returns the two ends of a new pseudo-terminal. The native check
// on FreeBSD is tests/integration/termpass; this test covers the logic on
// the development platform.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminals here:", err)
	}
	t.Cleanup(func() { master.Close() })
	unlock := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Skip("cannot unlock the pseudo-terminal:", errno)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip("cannot name the pseudo-terminal:", err)
	}
	slave, err = os.OpenFile("/dev/pts/"+itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("cannot open the pseudo-terminal:", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// screen collects what the terminal shows. A goroutine fills it: read
// deadlines do not work on a pseudo-terminal master.
type screen struct {
	mu  sync.Mutex
	buf strings.Builder
}

func watch(master *os.File) *screen {
	s := &screen{}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := master.Read(buf)
			s.mu.Lock()
			s.buf.Write(buf[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// readUntil waits until the terminal has shown want.
func readUntil(t *testing.T, shown *screen, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(shown.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never showed %q; it showed %q", want, shown.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func echoOn(t *testing.T, tty *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func TestPasswordIsNotEchoed(t *testing.T) {
	master, slave := openPTY(t)
	if !echoOn(t, slave) {
		t.Fatal("a fresh terminal should echo")
	}

	type result struct {
		pw  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		pw, err := ReadFrom(context.Background(), slave, "Password: ")
		done <- result{pw, err}
	}()

	shown := watch(master)
	readUntil(t, shown, "Password: ")
	if echoOn(t, slave) {
		t.Error("echo is still on while the password is being typed")
	}
	if _, err := master.WriteString("s3cret-passphrase\n"); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.pw != "s3cret-passphrase" {
		t.Fatalf("read %q, %v", r.pw, r.err)
	}
	readUntil(t, shown, "\n")
	if strings.Contains(shown.String(), "s3cret") {
		t.Errorf("the terminal showed the password: %q", shown.String())
	}
	if !echoOn(t, slave) {
		t.Error("echo was not switched back on")
	}
}

func TestCancelRestoresTheTerminal(t *testing.T) {
	master, slave := openPTY(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ReadFrom(ctx, slave, "Password: ")
		done <- err
	}()
	readUntil(t, watch(master), "Password: ")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFrom did not return after the cancel")
	}
	if !echoOn(t, slave) {
		t.Error("echo was left off after a cancelled read")
	}
}

func TestNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := ReadFrom(context.Background(), f, "Password: "); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("a regular file: %v, want ErrNoTerminal", err)
	}
}
