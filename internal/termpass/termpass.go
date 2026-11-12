//go:build freebsd || linux

// Package termpass reads a password from the terminal without echoing it.
//
// It talks to the controlling terminal (/dev/tty), not to standard input,
// so a password can be typed even when standard input is redirected, and
// is never read from a pipe by accident. Echo is switched off with
// termios through golang.org/x/sys/unix, which Skep already depends on.
package termpass

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrNoTerminal is returned when the process has no controlling terminal
// to ask on.
var ErrNoTerminal = errors.New("no terminal to read the password from")

// MaxLength bounds what is read: far above any password that is accepted.
const MaxLength = 4096

// Read prints prompt on the controlling terminal and reads one line from
// it with echo off. The terminal is restored before Read returns, also
// when ctx is cancelled (Ctrl-C).
func Read(ctx context.Context, prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoTerminal, err)
	}
	defer tty.Close()
	return ReadFrom(ctx, tty, prompt)
}

// ReadFrom is Read on a given terminal.
func ReadFrom(ctx context.Context, tty *os.File, prompt string) (string, error) {
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoTerminal, err)
	}
	quiet := *old
	// No echo of what is typed. Canonical mode and signals stay on, so
	// line editing and Ctrl-C work as usual.
	quiet.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return "", fmt.Errorf("cannot switch off terminal echo: %w", err)
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
	defer restore()

	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReaderSize(tty, MaxLength).ReadString('\n')
		done <- result{line, err}
	}()

	select {
	case <-ctx.Done():
		// The reader goroutine stays blocked until the process exits;
		// the terminal is restored by the deferred call.
		fmt.Fprintln(tty)
		return "", ctx.Err()
	case r := <-done:
		// The newline the user typed was not echoed.
		fmt.Fprintln(tty)
		if r.err != nil && r.line == "" {
			return "", fmt.Errorf("read password: %w", r.err)
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	}
}
