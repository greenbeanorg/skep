package raftnet

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"
)

// SendHeartbeat sends one Raft heartbeat (an AppendEntries on the
// receiver's heartbeat fast path) as the node nodeID at nodeAddr, with
// cert, to the listener at address whose key has serverPin, and reports
// whether the receiver answered it. A refusal by Skep's per-RPC check
// closes the connection without an answer. It is a diagnostic for tests
// (how long a removed node's heartbeat is still accepted, decision D11 of
// step 8b-2a); the service never calls it.
func SendHeartbeat(cert func() *tls.Certificate, address, serverPin, nodeID, nodeAddr string, term uint64, timeout time.Duration) error {
	tc, err := DialPinned(address, serverPin, cert, timeout)
	if err != nil {
		return err
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(timeout))
	w := bufio.NewWriter(tc)
	mp := &codec.MsgpackHandle{}
	mp.TimeNotBuiltin = true
	req := &raft.AppendEntriesRequest{
		RPCHeader: raft.RPCHeader{ProtocolVersion: ProtocolVersion, ID: []byte(nodeID), Addr: []byte(nodeAddr)},
		Term:      term,
	}
	if err := w.WriteByte(rpcAppendEntries); err != nil {
		return err
	}
	if err := codec.NewEncoder(w, mp).Encode(req); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var respErr string
	if err := codec.NewDecoder(bufio.NewReader(tc), &codec.MsgpackHandle{}).Decode(&respErr); err != nil {
		return err
	}
	if respErr != "" {
		return errors.New(respErr)
	}
	return nil
}

// Probe results of ProbeKey.
const (
	// ProbeAccepted: the node answered and accepts the key (its listener
	// sent the accepted marker after the handshake).
	ProbeAccepted = "accepts this key"
	// ProbeRefused: the node answered (its key has the expected pin) and
	// refused the key.
	ProbeRefused = "answers and refuses this key"
	// ProbeNoAnswer: no connection, or a key at the address that is not
	// the expected one.
	ProbeNoAnswer = "no answer"
)

// ProbeKey tells whether the node at address, whose key has serverPin,
// accepts cert's key. In TLS 1.3 the client's handshake ends before the
// server has checked the client's key, so the listener's first byte after
// the handshake is how acceptance is known (decision D9 of step 8b-2a):
// the accepted marker, or a refusal (an alert or a closed connection).
// The error says why for the two other results.
func ProbeKey(address, serverPin string, cert func() *tls.Certificate, timeout time.Duration) (string, error) {
	d := net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", address)
	if err != nil {
		return ProbeNoAnswer, err
	}
	tc := tls.Client(raw, ClientTLSConfig(serverPin, cert))
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(timeout))
	if err := tc.Handshake(); err != nil {
		// The server's key is not the expected one, or the connection
		// failed: not an answer from that node.
		return ProbeNoAnswer, err
	}
	if err := confirmAccepted(tc, timeout); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return ProbeNoAnswer, err
		}
		return ProbeRefused, err
	}
	return ProbeAccepted, nil
}
