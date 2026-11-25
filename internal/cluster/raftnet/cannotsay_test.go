package raftnet

import (
	"crypto/tls"
	"errors"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
)

// The listener's byte and the dialler's must be the same.
func TestCannotSayByteMatches(t *testing.T) {
	if peerlistener.CannotSay != cannotSayMarker || cannotSayMarker == acceptedMarker {
		t.Fatalf("listener %#x, dialler %#x, accepted %#x", peerlistener.CannotSay, cannotSayMarker, acceptedMarker)
	}
}

// csListener is a listener that accepts the pins in accept, groups keys by
// group, and serves an accepted connection as the transport does: the
// accepted marker, then it holds the connection until the client closes.
type csListener struct {
	l         *peerlistener.Listener
	server    *nodekey.Key
	cannotSay atomic.Bool
	mu        sync.Mutex
	accept    map[string]string // pin -> group
}

func newCSListener(t *testing.T) *csListener {
	t.Helper()
	c := &csListener{server: newKey(t, "00000000-0000-4000-8000-0000000000f0"), accept: map[string]string{}}
	l, err := peerlistener.Listen(peerlistener.Options{
		Address:     netip.MustParseAddrPort("127.0.0.1:0"),
		Certificate: c.server.Certificate,
		Accept: func(pin string) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			_, ok := c.accept[pin]
			return ok
		},
		CannotSay: c.cannotSay.Load,
		Group: func(pin string) (string, int) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.accept[pin], peerlistener.MaxPerNode
		},
		Handle: func(conn *tls.Conn, _ string) {
			_, _ = conn.Write([]byte{acceptedMarker})
			_, _ = io.Copy(io.Discard, conn)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	c.l = l
	return c
}

func (c *csListener) addr() string { return c.l.Addr().String() }

// D2: a key the node does not accept gets the silent close (refused) while
// the node can say, and the cannot-say byte while it cannot. Every dialler
// treats the byte as a failed connection; KeyAnswer and ProbeKey say
// "cannot say".
func TestCannotSayByte(t *testing.T) {
	c := newCSListener(t)
	member := newKey(t, "00000000-0000-4000-8000-0000000000f1")
	stranger := newKey(t, "00000000-0000-4000-8000-0000000000f2")
	c.accept[member.Pin()] = "node:a"

	if res, err := ProbeKey(c.addr(), c.server.Pin(), member.Certificate, 3*time.Second); res != ProbeAccepted {
		t.Fatalf("member: %s %v", res, err)
	}
	if res, err := ProbeKey(c.addr(), c.server.Pin(), stranger.Certificate, 3*time.Second); res != ProbeRefused {
		t.Fatalf("stranger, the node can say: %s %v", res, err)
	}
	c.cannotSay.Store(true)
	if res, err := ProbeKey(c.addr(), c.server.Pin(), stranger.Certificate, 3*time.Second); res != ProbeCannotSay || !errors.Is(err, ErrCannotSay) {
		t.Fatalf("stranger, the node cannot say: %s %v", res, err)
	}
	_, err := DialPinned(c.addr(), c.server.Pin(), stranger.Certificate, 3*time.Second)
	var he *HandshakeError
	if !errors.As(err, &he) || !he.Answered || KeyAnswer(err) != ProbeCannotSay {
		t.Fatalf("dial while the node cannot say: %v (answered %v), %s", err, he != nil && he.Answered, KeyAnswer(err))
	}
	// An accepted key is accepted whatever the node can say.
	if res, err := ProbeKey(c.addr(), c.server.Pin(), member.Certificate, 3*time.Second); res != ProbeAccepted {
		t.Fatalf("member while the node cannot say: %s %v", res, err)
	}
	c.cannotSay.Store(false)
	_, err = DialPinned(c.addr(), c.server.Pin(), stranger.Certificate, 3*time.Second)
	if KeyAnswer(err) != ProbeRefused {
		t.Fatalf("refused dial: %v %s", err, KeyAnswer(err))
	}
	// Nothing listening: no answer.
	_, err = DialPinned("127.0.0.1:1", c.server.Pin(), stranger.Certificate, time.Second)
	if KeyAnswer(err) != ProbeNoAnswer || KeyAnswer(nil) != ProbeAccepted {
		t.Fatalf("no listener: %v %s", err, KeyAnswer(err))
	}
}

// D5: the 17th connection of one member node gets the cannot-say byte, the
// 16 open ones stay open, and another member's connections still succeed.
// Both pins of a node (a rotating node) count as one node.
func TestPerNodeConnectionLimit(t *testing.T) {
	c := newCSListener(t)
	a1 := newKey(t, "00000000-0000-4000-8000-0000000000a1")
	a2 := newKey(t, "00000000-0000-4000-8000-0000000000a2") // a's second pin
	b := newKey(t, "00000000-0000-4000-8000-0000000000b1")
	c.accept[a1.Pin()], c.accept[a2.Pin()], c.accept[b.Pin()] = "node:a", "node:a", "node:b"
	var open []*tls.Conn
	defer func() {
		for _, x := range open {
			x.Close()
		}
	}()
	for i := 0; i < peerlistener.MaxPerNode; i++ {
		k := a1
		if i%2 == 1 {
			k = a2
		}
		conn, err := DialPinned(c.addr(), c.server.Pin(), k.Certificate, 3*time.Second)
		if err != nil {
			t.Fatalf("connection %d of node a: %v", i+1, err)
		}
		open = append(open, conn)
	}
	_, err := DialPinned(c.addr(), c.server.Pin(), a1.Certificate, 3*time.Second)
	if KeyAnswer(err) != ProbeCannotSay {
		t.Fatalf("the 17th connection of node a: %v (%s)", err, KeyAnswer(err))
	}
	if res, _ := ProbeKey(c.addr(), c.server.Pin(), a2.Certificate, 3*time.Second); res != ProbeCannotSay {
		t.Fatalf("a's other pin over the limit: %s", res)
	}
	for i := 0; i < 3; i++ {
		conn, err := DialPinned(c.addr(), c.server.Pin(), b.Certificate, 3*time.Second)
		if err != nil {
			t.Fatalf("node b while a is at its limit: %v", err)
		}
		open = append(open, conn)
	}
	if got := c.l.OpenPerGroup(); got["node:a"] != peerlistener.MaxPerNode || got["node:b"] != 3 {
		t.Fatalf("open per node: %v", got)
	}
	// The open connections of a were not touched: one still carries data.
	if _, err := open[0].Write([]byte("x")); err != nil {
		t.Fatalf("an open connection of a was closed: %v", err)
	}
	// One of a's closes: the next one is accepted again.
	open[0].Close()
	open = open[1:]
	deadline := time.Now().Add(5 * time.Second)
	for c.l.OpenPerGroup()["node:a"] >= peerlistener.MaxPerNode && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := DialPinned(c.addr(), c.server.Pin(), a1.Certificate, 3*time.Second)
	if err != nil {
		t.Fatalf("after one of a's closed: %v", err)
	}
	open = append(open, conn)
}
