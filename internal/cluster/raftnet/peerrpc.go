package raftnet

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"bytes"
)

// Skep's own RPCs share port 8444 with Raft's. A connection that starts
// with peerRPCMagic carries one Skep RPC: a 4-byte big-endian length and a
// JSON request, answered the same way, then the connection is closed.
const (
	peerRPCMagic byte = 'S'
	// acceptedMarker is the byte the listener sends first on every
	// connection it accepted. In TLS 1.3 the client's handshake finishes
	// before the server has checked the client's key, so without it a
	// client could not tell a refused key from an accepted one before it
	// sent something.
	acceptedMarker byte = 0x01
	// maxPeerMessage bounds a Skep RPC request or answer.
	maxPeerMessage = 256 << 10
	// PeerTimeout bounds one Skep RPC, the join call's write through the
	// log included.
	PeerTimeout = 30 * time.Second
)

// Error codes of Skep RPCs.
const (
	CodeForbidden      = "forbidden"
	CodeInvalidRequest = "invalid_request"
	CodeNotLeader      = "not_leader"
	CodeRefused        = "refused"
	CodeOutcomeUnknown = "outcome_unknown"
	CodeUnavailable    = "unavailable"
)

type peerRequest struct {
	Op     string          `json:"op"`
	NodeID string          `json:"node_id"`
	Body   json.RawMessage `json:"body"`
}

type peerResponse struct {
	OK      bool            `json:"ok"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Leader  *LeaderHint     `json:"leader,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// LeaderHint names the leader in a not_leader answer.
type LeaderHint struct {
	NodeID  string `json:"node_id"`
	Address string `json:"address"`
}

// PeerError is an error answer of a Skep RPC.
type PeerError struct {
	Code    string
	Message string
	Leader  *LeaderHint
}

func (e *PeerError) Error() string { return e.Code + ": " + e.Message }

// OutcomeUnknownError means the request was sent and no answer came: the
// receiver may or may not have acted on it (ADR 0013 B11).
type OutcomeUnknownError struct{ Err error }

func (e *OutcomeUnknownError) Error() string {
	return "outcome unknown: the request was sent and no answer came: " + e.Err.Error()
}

func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// PeerHandler serves Skep's own RPCs. The transport has already checked
// that the caller's key may call the operation.
type PeerHandler interface {
	HandlePeer(ctx context.Context, op string, from Peer, body json.RawMessage) (any, error)
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxPeerMessage {
		return fmt.Errorf("message of %d bytes is over the limit", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxPeerMessage {
		return fmt.Errorf("message of %d bytes is outside the limits", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// servePeerRPC serves the one Skep RPC of a connection.
func (t *Transport) servePeerRPC(conn net.Conn, r *bufio.Reader, pin string) {
	_ = conn.SetDeadline(time.Now().Add(PeerTimeout))
	answer := func(resp peerResponse) {
		_ = writeFrame(conn, resp)
	}
	var req peerRequest
	if err := readFrame(r, &req); err != nil {
		answer(peerResponse{Code: CodeInvalidRequest, Message: "cannot read the request: " + err.Error()})
		return
	}
	p, ok := t.trust.ByPin(pin)
	if !ok {
		answer(peerResponse{Code: CodeForbidden, Message: "the key is no longer accepted"})
		return
	}
	if req.NodeID == "" || req.NodeID != p.NodeID {
		t.log.Warn("node listener: rpc refused, header claims another node", "remote", conn.RemoteAddr().String(),
			"op", req.Op, "header_id", req.NodeID, "key_node", p.NodeID)
		answer(peerResponse{Code: CodeForbidden, Message: "the request claims another node than the key's"})
		return
	}
	if _, ok := Allowed(req.Op, p.Kind); !ok {
		t.log.Warn("node listener: rpc refused for this kind of key", "remote", conn.RemoteAddr().String(),
			"op", req.Op, "node_id", p.NodeID, "kind", p.Kind.String())
		answer(peerResponse{Code: CodeForbidden, Message: fmt.Sprintf("the operation %q is not open to a %s key", req.Op, p.Kind)})
		return
	}
	if t.peer == nil {
		answer(peerResponse{Code: CodeUnavailable, Message: "this node serves no Skep RPC"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), PeerTimeout-2*time.Second)
	defer cancel()
	out, err := t.peer.HandlePeer(ctx, req.Op, p, req.Body)
	if err != nil {
		var pe *PeerError
		if errors.As(err, &pe) {
			answer(peerResponse{Code: pe.Code, Message: pe.Message, Leader: pe.Leader})
		} else {
			answer(peerResponse{Code: CodeUnavailable, Message: err.Error()})
		}
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		answer(peerResponse{Code: CodeUnavailable, Message: err.Error()})
		return
	}
	answer(peerResponse{OK: true, Body: body})
}

// confirmAccepted reads the listener's accepted marker. Until it has come,
// nothing is sent.
func confirmAccepted(tc *tls.Conn, timeout time.Duration) error {
	_ = tc.SetReadDeadline(time.Now().Add(timeout))
	var b [1]byte
	if _, err := io.ReadFull(tc, b[:]); err != nil {
		return err
	}
	if b[0] != acceptedMarker {
		return fmt.Errorf("unexpected first byte %#x from the listener", b[0])
	}
	_ = tc.SetReadDeadline(time.Time{})
	return nil
}

// Caller makes Skep RPCs as this node.
type Caller struct {
	NodeID      string
	Certificate func() *tls.Certificate
}

// Call sends one Skep RPC to the node at address whose key has the given
// pin, and decodes the answer into out. The error is a *HandshakeError when
// nothing was sent (the connection, the handshake or the listener refused
// the key), an *OutcomeUnknownError when the request was sent and no answer
// came, and a *PeerError for an error answer.
func (c Caller) Call(ctx context.Context, address, pin, op string, in, out any) error {
	timeout := dialTimeout
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < timeout {
		timeout = time.Until(dl)
	}
	tc, err := DialPinned(address, pin, c.Certificate, timeout)
	if err != nil {
		var he *HandshakeError
		if errors.As(err, &he) {
			return err
		}
		return &HandshakeError{Address: address, Err: err}
	}
	defer tc.Close()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(PeerTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = tc.SetDeadline(deadline)
	if _, err := tc.Write([]byte{peerRPCMagic}); err != nil {
		return &HandshakeError{Address: address, Err: err}
	}
	if err := writeFrame(tc, peerRequest{Op: op, NodeID: c.NodeID, Body: body}); err != nil {
		return &OutcomeUnknownError{Err: err}
	}
	var resp peerResponse
	if err := readFrame(tc, &resp); err != nil {
		return &OutcomeUnknownError{Err: err}
	}
	if !resp.OK {
		return &PeerError{Code: resp.Code, Message: resp.Message, Leader: resp.Leader}
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
