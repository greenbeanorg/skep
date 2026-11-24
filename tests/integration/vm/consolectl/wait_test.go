package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func feed(chunks ...string) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		if len(chunks) == 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		c := chunks[0]
		chunks = chunks[1:]
		if c == "EOF" {
			return nil, io.EOF
		}
		return []byte(c), nil
	}
}

func TestWaitForMatchesAcrossMessages(t *testing.T) {
	if _, err := waitFor(context.Background(), feed("FreeBSD/amd64 (guest) (ttyu0)\r\n\r\nlog", "in: "), []byte("login:")); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForTimeoutReturnsTheTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	tail, err := waitFor(ctx, feed("booting", "\x1b[2J\x07still booting"), []byte("login:"))
	if !errors.Is(err, errNotSeen) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	got := escaped(tail)
	if got != `"booting\x1b[2J\astill booting"` {
		t.Errorf("escaped = %s", got)
	}
}

func TestWaitForConsoleClosed(t *testing.T) {
	tail, err := waitFor(context.Background(), feed("panic", "EOF"), []byte("login:"))
	if !errors.Is(err, errNotSeen) || !errors.Is(err, io.EOF) || string(tail) != "panic" {
		t.Fatalf("tail %q, err %v", tail, err)
	}
}

// The text kept is bounded, and a pattern split at the boundary of what
// is kept still matches.
func TestWaitForKeepsABoundedTail(t *testing.T) {
	big := strings.Repeat("x", 3*tailBytes)
	tail, err := waitFor(context.Background(), feed(big, "lo", "gin:"), []byte("login:"))
	if err != nil || len(tail) > tailBytes {
		t.Fatalf("len %d, err %v", len(tail), err)
	}
}

func TestEscapedShowsNoControlBytes(t *testing.T) {
	s := escaped([]byte("a\x1b]0;title\x07\r\n\xffé" + strings.Repeat("y", 2*shownBytes)))
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("unescaped byte %q in %s", r, s)
		}
	}
	if len(s) > shownBytes+2 {
		t.Errorf("shown %d bytes", len(s))
	}
}

func TestWaitForRejectsAnEmptyPattern(t *testing.T) {
	if _, err := waitFor(context.Background(), feed("x"), nil); err == nil {
		t.Fatal("empty pattern accepted")
	}
}
