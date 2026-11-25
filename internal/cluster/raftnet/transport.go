package raftnet

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
)

// Bounds of the node-to-node protocol.
const (
	// ProtocolVersion is the only Raft protocol version accepted: the
	// one hashicorp/raft v1.8.0 speaks by default. Every RPC of it
	// carries the sender's ID in its header.
	ProtocolVersion = raft.ProtocolVersion(3)
	// idleTimeout is how long an accepted connection may wait for its
	// next RPC.
	idleTimeout = 5 * time.Minute
	// rpcTimeout bounds reading one RPC once its first byte has come, and
	// writing its answer.
	rpcTimeout = 30 * time.Second
	// maxRPCSize bounds the bytes read for one RPC other than a
	// snapshot's data.
	maxRPCSize = 32 << 20
	// maxSnapshotSize bounds the snapshot data one InstallSnapshot may
	// stream. Skep's replicated store is small; this is far above it.
	maxSnapshotSize = 256 << 20
	// dialTimeout bounds a TCP connect plus the TLS handshake.
	dialTimeout = 10 * time.Second
	// CacheAge is how old the Raft configuration used on the heartbeat
	// fast path may be.
	CacheAge = 250 * time.Millisecond
	// maxInitLen bounds what the decoder allocates up front for a
	// collection whose length the sender declares.
	maxInitLen = 1 << 16
	// poolSize is the library's connection pool per peer.
	poolSize = 3
)

// snapshotTimeout scales the read deadline with the snapshot's size, as the
// library's sender does (10 s per 256 KiB, at least rpcTimeout).
func snapshotTimeout(size int64) time.Duration {
	d := 10 * time.Second * time.Duration(size/(256*1024))
	if d < rpcTimeout {
		d = rpcTimeout
	}
	return d
}

func decodeHandle() *codec.MsgpackHandle {
	h := &codec.MsgpackHandle{}
	h.MaxInitLen = maxInitLen
	return h
}

// budgetReader fails a read once more bytes than its budget were read since
// the last reset. It bounds what one RPC may make the node read.
type budgetReader struct {
	r    io.Reader
	left int64
}

func (b *budgetReader) reset(n int64) { b.left = n }

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("rpc is over the size limit")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// Options configures the transport.
type Options struct {
	// LocalAddress is this node's address ("ip:port"), as in its member
	// entry. The library puts it in every RPC header it sends.
	LocalAddress string
	// Certificate returns this node's current certificate.
	Certificate func() *tls.Certificate
	// Trust answers who a key is.
	Trust Trust
	// Peer serves Skep's own RPCs. Nil refuses them all.
	Peer PeerHandler
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Transport is a raft.Transport: outbound through the library's
// NetworkTransport, inbound served by Serve, which the node-to-node
// listener calls for each accepted connection.
type Transport struct {
	*raft.NetworkTransport

	local string
	cert  func() *tls.Certificate
	trust Trust
	peer  PeerHandler
	log   *slog.Logger

	consumeCh chan raft.RPC
	hbMu      sync.Mutex
	hbFn      func(raft.RPC)
	shutdown  chan struct{}
	once      sync.Once
}

// New returns the transport. It opens no socket: inbound connections come
// from the node-to-node listener (Serve), outbound ones are dialled when
// the library sends an RPC.
func New(o Options) (*Transport, error) {
	if o.LocalAddress == "" || o.Certificate == nil || o.Trust == nil {
		return nil, errors.New("raftnet: a local address, a certificate and a trust source are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	t := &Transport{
		local: o.LocalAddress, cert: o.Certificate, trust: o.Trust, peer: o.Peer, log: o.Logger,
		consumeCh: make(chan raft.RPC), shutdown: make(chan struct{}),
	}
	t.NetworkTransport = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  streamLayer{t},
		MaxPool: poolSize,
		Timeout: dialTimeout,
		Logger:  hclog.New(&hclog.LoggerOptions{Name: "raft-net", Level: hclog.Warn, Output: io.Discard}),
	})
	return t, nil
}

// Consumer implements raft.Transport: the RPCs Serve accepted.
func (t *Transport) Consumer() <-chan raft.RPC { return t.consumeCh }

// SetHeartbeatHandler implements raft.Transport.
func (t *Transport) SetHeartbeatHandler(cb func(rpc raft.RPC)) {
	t.hbMu.Lock()
	t.hbFn = cb
	t.hbMu.Unlock()
}

// LocalAddr implements raft.Transport.
func (t *Transport) LocalAddr() raft.ServerAddress { return raft.ServerAddress(t.local) }

// Close implements raft.WithClose.
func (t *Transport) Close() error {
	t.once.Do(func() { close(t.shutdown) })
	return t.NetworkTransport.Close()
}

// checkSender is the per-RPC identity check (ADR 0013 B1): the header's
// node ID has an entry whose pin is the key of this connection, a key of
// that kind may make Raft RPCs, the header's address is that node's, and
// the node is a server in the receiver's latest Raft configuration. The
// TCP source address is not compared.
func (t *Transport) checkSender(pin string, hdr raft.RPCHeader, heartbeat bool) error {
	if hdr.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("raft protocol version %d is not accepted (only %d)", hdr.ProtocolVersion, ProtocolVersion)
	}
	p, ok := t.trust.ByPin(pin)
	if !ok {
		return errors.New("the key is no longer accepted")
	}
	if len(hdr.ID) == 0 || string(hdr.ID) != p.NodeID {
		return fmt.Errorf("the header claims node %q, the key belongs to %s", hdr.ID, p.NodeID)
	}
	if _, ok := Allowed(OpRaft, p.Kind); !ok {
		return fmt.Errorf("a %s key may not make Raft RPCs", p.Kind)
	}
	if string(hdr.Addr) != p.Address {
		return fmt.Errorf("the header claims address %q, node %s has %q", hdr.Addr, p.NodeID, p.Address)
	}
	if !t.trust.RaftServer(p.NodeID, heartbeat) {
		return fmt.Errorf("node %s is not a server in the Raft configuration", p.NodeID)
	}
	return nil
}

// mayMakeRaftRPCs: the key is accepted and its kind may make Raft RPCs.
func (t *Transport) mayMakeRaftRPCs(pin string) error {
	p, ok := t.trust.ByPin(pin)
	if !ok {
		return errors.New("the key is no longer accepted")
	}
	if _, ok := Allowed(OpRaft, p.Kind); !ok {
		return fmt.Errorf("a %s key may not make Raft RPCs", p.Kind)
	}
	return nil
}

func (t *Transport) refused(conn net.Conn, pin string, rpcType uint8, hdr raft.RPCHeader, err error) {
	t.log.Warn("node listener: raft rpc refused", "remote", conn.RemoteAddr().String(), "pin", pin,
		"rpc_type", rpcType, "header_id", string(hdr.ID), "error", err.Error())
}

// Serve serves one accepted connection: Raft RPCs if its first byte is a
// Raft RPC type, one Skep RPC if it is peerRPCMagic, nothing otherwise.
func (t *Transport) Serve(conn *tls.Conn, pin string) {
	_ = conn.SetWriteDeadline(time.Now().Add(rpcTimeout))
	if _, err := conn.Write([]byte{acceptedMarker}); err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})
	br := &budgetReader{r: conn, left: maxRPCSize}
	r := bufio.NewReaderSize(br, connReceiveBufferSize)
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	first, err := r.Peek(1)
	if err != nil {
		return
	}
	switch {
	case isRaftType(first[0]):
		// A pending key is refused before any RPC is read or decoded.
		if err := t.mayMakeRaftRPCs(pin); err != nil {
			t.log.Warn("node listener: raft connection refused before any rpc was read", "remote", conn.RemoteAddr().String(), "pin", pin, "error", err.Error())
			return
		}
		t.serveRaft(conn, br, r, pin)
	case first[0] == peerRPCMagic:
		_, _ = r.ReadByte()
		t.servePeerRPC(conn, r, pin)
	default:
		t.log.Warn("node listener: unknown protocol byte; closing", "remote", conn.RemoteAddr().String(), "byte", first[0])
	}
}

// streamLayer is the outbound side handed to raft.NetworkTransport. Its
// Accept never yields a connection: inbound connections are served by
// Serve.
type streamLayer struct{ t *Transport }

func (s streamLayer) Accept() (net.Conn, error) {
	<-s.t.shutdown
	return nil, errors.New("transport closed")
}

func (s streamLayer) Close() error { return nil }

func (s streamLayer) Addr() net.Addr { return addrString(s.t.local) }

// Dial connects to the node the address belongs to and accepts only that
// node's pin. The library hands the dialler an address, never a server ID
// (net_transport.go getConn), so the node is found by its address, which is
// unique among the keys the node accepts (checked at apply).
func (s streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	p, ok := s.t.trust.ByAddress(string(address))
	if !ok {
		return nil, fmt.Errorf("raftnet: no member is known at %s", address)
	}
	if timeout <= 0 || timeout > dialTimeout {
		timeout = dialTimeout
	}
	// Either pin of a rotating node (D1 of step 8b-2b); which one it
	// used is reported.
	c, seen, err := DialPins(string(address), p.Pins(), s.t.cert, timeout)
	if err == nil {
		if o, ok := s.t.trust.(PinObserver); ok {
			o.SawPin(p.NodeID, seen)
		}
	}
	return c, err
}

type addrString string

func (a addrString) Network() string { return "tcp" }
func (a addrString) String() string  { return string(a) }

// ClientTLSConfig is the dialler's TLS configuration: TLS 1.3 only, this
// node's certificate, and the server accepted only if its key has the
// expected pin (and its certificate is in its validity period).
func ClientTLSConfig(pin string, cert func() *tls.Certificate) *tls.Config {
	return ClientTLSConfigPins([]string{pin}, cert, nil)
}

// ClientTLSConfigPins is ClientTLSConfig accepting any of pins (the two
// pins of a rotating node); seen, when not nil, is set to the pin the
// server's key had.
func ClientTLSConfigPins(pins []string, cert func() *tls.Certificate, seen *string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// No chain: the pin is the trust, checked in VerifyConnection.
		InsecureSkipVerify: true, //nolint:gosec // verified by pin below
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c := cert()
			if c == nil {
				return nil, errors.New("no node certificate")
			}
			return c, nil
		},
		SessionTicketsDisabled: true,
		ClientSessionCache:     nil,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 {
				return errors.New("TLS 1.3 is required")
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return errors.New("the server certificate is expired or not yet valid")
			}
			got := nodekey.PinOfCertificate(leaf)
			for _, p := range pins {
				if p != "" && got == p {
					if seen != nil {
						*seen = got
					}
					return nil
				}
			}
			return fmt.Errorf("the server's key has pin %s, expected %s", got, strings.Join(pins, " or "))
		},
	}
}

// DialPinned connects to address and completes a TLS 1.3 handshake that
// accepts only the given pin.
func DialPinned(address, pin string, cert func() *tls.Certificate, timeout time.Duration) (*tls.Conn, error) {
	c, _, err := DialPins(address, []string{pin}, cert, timeout)
	return c, err
}

// DialPins is DialPinned accepting any of pins, and returning the pin the
// server's key had.
func DialPins(address string, pins []string, cert func() *tls.Certificate, timeout time.Duration) (*tls.Conn, string, error) {
	tc, seen, err := dialPins(address, pins, cert, timeout)
	return tc, seen, err
}

func dialPins(address string, pins []string, cert func() *tls.Certificate, timeout time.Duration) (*tls.Conn, string, error) {
	var seen string
	d := net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", address)
	if err != nil {
		return nil, "", err
	}
	tc := tls.Client(raw, ClientTLSConfigPins(pins, cert, &seen))
	_ = tc.SetDeadline(time.Now().Add(timeout))
	if err := tc.Handshake(); err != nil {
		_ = tc.Close()
		return nil, "", &HandshakeError{Address: address, Err: err}
	}
	// In TLS 1.3 the listener checks this node's key after the client's
	// handshake has finished; its marker says it accepted the key.
	if err := confirmAccepted(tc, timeout); err != nil {
		_ = tc.Close()
		return nil, "", &HandshakeError{Address: address, Err: err, Answered: true}
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, seen, nil
}

// HandshakeError is a failed TLS handshake: nothing was sent.
type HandshakeError struct {
	Address string
	Err     error
	// Answered is set when the TLS handshake with the expected key
	// completed and the listener's first byte (or its refusal) was
	// awaited: the node at the address is the one dialled.
	Answered bool
}

// KeyAnswer classifies a connection's outcome as ProbeKey does: nil, an
// error answer of a Skep RPC (*PeerError) or an unknown outcome after the
// request was sent is ProbeAccepted; a refusal after a completed handshake is
// ProbeRefused; the cannot-say byte is ProbeCannotSay; everything else,
// a timeout included, is ProbeNoAnswer.
func KeyAnswer(err error) string {
	if err == nil {
		return ProbeAccepted
	}
	var pe *PeerError
	var ou *OutcomeUnknownError
	if errors.As(err, &pe) || errors.As(err, &ou) {
		return ProbeAccepted // the connection was accepted; the RPC failed later
	}
	var he *HandshakeError
	if !errors.As(err, &he) {
		return ProbeNoAnswer
	}
	if !he.Answered {
		return ProbeNoAnswer
	}
	var ne net.Error
	switch {
	case errors.Is(he.Err, ErrCannotSay):
		return ProbeCannotSay
	case errors.As(he.Err, &ne) && ne.Timeout():
		return ProbeNoAnswer
	}
	return ProbeRefused
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("TLS handshake with %s failed: %v", e.Address, e.Err)
}

func (e *HandshakeError) Unwrap() error { return e.Err }

var (
	_ raft.Transport   = (*Transport)(nil)
	_ raft.WithClose   = (*Transport)(nil)
	_ raft.WithPreVote = (*Transport)(nil)
)
