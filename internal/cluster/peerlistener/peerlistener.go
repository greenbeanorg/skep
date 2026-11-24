// Package peerlistener is skep-cluster's node-to-node listener on TCP
// port 8444 (ADR 0013 B1, "What the listener accepts"): TLS 1.3 only, with
// a client certificate whose key is pinned in the membership. Every other
// key is refused in the handshake.
//
// In step 8a the membership is this node alone, there is no join and no
// inbound Raft RPC handler: an accepted connection (only this node's own
// key can be) is closed after the handshake without any data.
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
)

// Options configures the listener.
type Options struct {
	// Address is the one address and port to bind.
	Address netip.AddrPort
	// Certificate returns this node's current certificate.
	Certificate func() *tls.Certificate
	// Pinned reports whether a key's pin is in the membership the node
	// has applied, and whose it is. It is asked at every handshake.
	Pinned func(pin string) (nodeID string, ok bool)
	// Handle serves an accepted connection from a pinned key. Nil
	// closes it at once, which is all step 8a does.
	Handle func(conn *tls.Conn, nodeID string)
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
			if _, ok := o.Pinned(nodekey.PinOfCertificate(leaf)); !ok {
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
	if o.Certificate == nil || o.Pinned == nil {
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
	l := &Listener{ln: ln, conf: TLSConfig(o), opts: o, sem: make(chan struct{}, maxHandshakes)}
	l.wg.Add(1)
	go l.accept()
	return l, nil
}

// Addr returns the bound address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops accepting and waits for the connections in progress.
func (l *Listener) Close() error {
	err := l.ln.Close()
	l.wg.Wait()
	return err
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
		go func() {
			defer l.wg.Done()
			defer func() { <-l.sem }()
			l.serve(c)
		}()
	}
}

func (l *Listener) serve(c net.Conn) {
	defer c.Close()
	tc := tls.Server(c, l.conf)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		l.opts.Logger.Warn("node listener: connection refused in the handshake",
			"remote", c.RemoteAddr().String(), "error", err.Error())
		return
	}
	cs := tc.ConnectionState()
	nodeID, ok := l.opts.Pinned(nodekey.PinOfCertificate(cs.PeerCertificates[0]))
	if !ok {
		return
	}
	if l.opts.Handle == nil {
		l.opts.Logger.Info("node listener: pinned key connected; nothing is served to it in this build",
			"remote", c.RemoteAddr().String(), "node_id", nodeID)
		_ = tc.Close()
		return
	}
	l.opts.Handle(tc, nodeID)
}
