//go:build !freebsd && !linux

// Package termpass reads a password from the terminal without echoing it.
// It is implemented on FreeBSD (production) and Linux (development) only.
package termpass

import (
	"context"
	"errors"
)

// ErrNoTerminal is returned when there is no terminal to ask on.
var ErrNoTerminal = errors.New("no terminal to read the password from")

// Read is not implemented on this operating system.
func Read(context.Context, string) (string, error) {
	return "", errors.New("reading a password from the terminal is not supported on this operating system; use --password-stdin")
}
