package main

import (
	"bytes"
	"context"
	"errors"
	"strconv"
)

// tailBytes is how much console text -wait-for keeps: enough to match a
// pattern split across messages and to show the end of the output on a
// timeout.
const tailBytes = 4096

// shownBytes is how much of that tail a timeout prints.
const shownBytes = 512

// errNotSeen is returned by waitFor when the console closed, or the
// context ended, before the pattern appeared.
var errNotSeen = errors.New("the pattern did not appear")

// waitFor reads console messages until pattern appears in the text, and
// returns the last tailBytes of it. Console text comes from the guest and
// is untrusted: it is only searched, as bytes, for a literal pattern;
// nothing in it is interpreted or acted on. It returns nil on a match,
// and otherwise errNotSeen joined with the reason (the context's error, or
// the read error).
func waitFor(ctx context.Context, read func(context.Context) ([]byte, error), pattern []byte) ([]byte, error) {
	if len(pattern) == 0 || len(pattern) > tailBytes {
		return nil, errors.New("the pattern must be 1 to 4096 bytes")
	}
	var tail []byte
	for {
		data, err := read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return tail, errors.Join(errNotSeen, err)
		}
		tail = append(tail, data...)
		if bytes.Contains(tail, pattern) {
			return keepTail(tail), nil
		}
		tail = keepTail(tail)
	}
}

func keepTail(b []byte) []byte {
	if len(b) <= tailBytes {
		return b
	}
	return append([]byte(nil), b[len(b)-tailBytes:]...)
}

// escaped returns the end of the console text as a quoted ASCII string:
// control characters, escape sequences and non-ASCII bytes are shown as Go
// escapes, so the guest's output can never drive the reader's terminal.
func escaped(tail []byte) string {
	if len(tail) > shownBytes {
		tail = tail[len(tail)-shownBytes:]
	}
	return strconv.QuoteToASCII(string(tail))
}
