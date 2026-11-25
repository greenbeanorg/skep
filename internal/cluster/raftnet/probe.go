package raftnet

import (
	"bufio"
	"crypto/tls"
	"errors"
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
