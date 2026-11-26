package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/greenbeanorg/skep/internal/termpass"
)

// confirmOnTerminal asks a yes/no question on the controlling terminal,
// not on standard input, so that a piped input cannot answer it by
// accident. Only "y" or "yes" is a yes.
func confirmOnTerminal(ctx context.Context, prompt string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("%w: %v", termpass.ErrNoTerminal, err)
	}
	defer tty.Close()
	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return false, err
	}
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReaderSize(tty, 256).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(tty)
		return false, ctx.Err()
	case line := <-answer:
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes", nil
	}
}

// lineOnTerminal reads one line from the controlling terminal, not from
// standard input.
func lineOnTerminal(ctx context.Context, prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("%w: %v", termpass.ErrNoTerminal, err)
	}
	defer tty.Close()
	if _, err := fmt.Fprint(tty, prompt); err != nil {
		return "", err
	}
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReaderSize(tty, 256).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(tty)
		return "", ctx.Err()
	case line := <-answer:
		return strings.TrimSpace(line), nil
	}
}
