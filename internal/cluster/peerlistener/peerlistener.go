// Package peerlistener is skep-cluster's node-to-node listener on TCP
// port 8444 (ADR 0013 B1, "What the listener accepts"): TLS 1.3 only, with
// a client certificate whose key is pinned in the membership the node has
// applied (a member entry or a pending entry). Every other key is refused
// in the handshake. What an accepted key may do is decided per RPC by the
// handler (package raftnet).
package peerlistener

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
)

const (
	// handshakeTimeout bounds one handshake.
	handshakeTimeout = 10 * time.Second
	// maxHandshakes bounds the handshakes in progress.
	maxHandshakes = 64
	// MaxConnections bounds the accepted connections open at once.
	MaxConnections = 256
)

// Options configures the listener.
type Options struct {
	// Address is the one address and port to bind.
	Address netip.AddrPort
	// Certificate returns this node's current certificate.
	Certificate func() *tls.Certificate
	// Accept reports whether a key's pin is accepted now: pinned in a
	// member entry or a pending entry of the membership the node has
	// applied (ADR 0013 B1). It is asked at every handshake and again by
	// Revalidate.
	Accept func(pin string) bool
	// Handle serves an accepted connection. Nil closes it at once.
	Handle func(conn *tls.Conn, pin string)
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Listener is the open node-to-node listener.
type Listener struct {
	ln   net.Listener
	conf *tls.Config
	opts Options
	wg   sync.WaitGroup
	sem  chan struct{}

	mu     sync.Mutex
	conns  map[*tls.Conn]string
	closed bool
}

// errNotPinned is the handshake failure for a key outside the membership.
var errNotPinned = errors.New("the client key is not pinned in the cluster membership")

// TLSConfig returns the server-side TLS configuration: TLS 1.3 only, a
// client certificate required, accepted by its pin alone.
func TLSConfig(o Options) *tls.Config {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Any certificate is asked for; trust comes from the pin, not
		// from a chain, so no CA list is sent or used.
		ClientAuth: tls.RequireAnyClientCert,
		// No session tickets: every connection proves its key again,
		// against the membership as it is then.
		SessionTicketsDisabled: true,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := o.Certificate()
			if c == nil {
				return nil, errors.New("no node certificate")
			}
			return c, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 {
				return errors.New("TLS 1.3 is required")
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("a client certificate is required")
			}
			leaf := cs.PeerCertificates[0]
			// The pin carries the trust, but an expired certificate
			// is refused all the same, as Go's verifier would.
			if t := now(); t.Before(leaf.NotBefore) || t.After(leaf.NotAfter) {
				return errors.New("the client certificate is expired or not yet valid")
			}
			if !o.Accept(nodekey.PinOfCertificate(leaf)) {
				return errNotPinned
			}
			return nil
		},
	}
}

// Listen binds the address and starts accepting.
func Listen(o Options) (*Listener, error) {
	if !o.Address.IsValid() || o.Address.Addr().IsUnspecified() {
		return nil, errors.New("peerlistener: the listen address must be one specific address")
	}
	if o.Certificate == nil || o.Accept == nil {
		return nil, errors.New("peerlistener: a certificate and a pin check are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	network := "tcp4"
	if o.Address.Addr().Is6() {
		network = "tcp6"
	}
	ln, err := net.Listen(network, o.Address.String())
	if err != nil {
		return nil, fmt.Errorf("peerlistener: %w", err)
	}
	l := &Listener{ln: ln, conf: TLSConfig(o), opts: o, sem: make(chan struct{}, maxHandshakes), conns: map[*tls.Conn]string{}}
	l.wg.Add(1)
	go l.accept()
	return l, nil
}

// Addr returns the bound address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops accepting, closes the open connections and waits for their
// handlers.
func (l *Listener) Close() error {
	err := l.ln.Close()
	l.mu.Lock()
	// From here no connection is registered: one whose handshake ends
	// after this point is closed at once (it would otherwise be served
	// and keep Close waiting for as long as its peer keeps sending).
	l.closed = true
	for c := range l.conns {
		_ = c.Close()
	}
	l.mu.Unlock()
	l.wg.Wait()
	return err
}

// Revalidate closes every open connection whose key is no longer accepted
// (ADR 0013 B1: when a node applies a withdrawal or a pending entry's
// expiry, it closes that key's open connections). It returns how many it
// closed.
func (l *Listener) Revalidate() int {
	l.mu.Lock()
	var drop []*tls.Conn
	for c, pin := range l.conns {
		if !l.opts.Accept(pin) {
			drop = append(drop, c)
		}
	}
	l.mu.Unlock()
	for _, c := range drop {
		l.opts.Logger.Info("node listener: closing a connection whose key is no longer accepted", "remote", c.RemoteAddr().String())
		_ = c.Close()
	}
	return len(drop)
}

// Open returns the number of accepted connections open now.
func (l *Listener) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

func (l *Listener) accept() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.opts.Logger.Warn("node listener: accept failed", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case l.sem <- struct{}{}:
		default:
			// Too many handshakes in progress: drop, do not queue.
			c.Close()
			continue
		}
		l.wg.Add(1)
		go l.serve(c)
	}
}

func (l *Listener) serve(c net.Conn) {
	defer l.wg.Done()
	tc := tls.Server(c, l.conf)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	<-l.sem
	if err != nil {
		l.opts.Logger.Warn("node listener: connection refused in the handshake",
			"remote", c.RemoteAddr().String(), "error", err.Error())
		_ = c.Close()
		return
	}
	pin := nodekey.PinOfCertificate(tc.ConnectionState().PeerCertificates[0])
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = tc.Close()
		return
	}
	if len(l.conns) >= MaxConnections {
		l.mu.Unlock()
		l.opts.Logger.Warn("node listener: too many connections; closing", "remote", c.RemoteAddr().String())
		_ = tc.Close()
		return
	}
	l.conns[tc] = pin
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.conns, tc)
		l.mu.Unlock()
		_ = tc.Close()
	}()
	if l.opts.Handle == nil {
		return
	}
	l.opts.Handle(tc, pin)
}
