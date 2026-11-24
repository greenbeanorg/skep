// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Copyright IBM Corp. 2013, 2025 (the original handleConn and
// handleCommand in net_transport.go of github.com/hashicorp/raft v1.8.0).
// Adapted for Skep, 2026: serveRaft and handleRaftRPC below.
//
// What differs from upstream (compare at every library update; the
// checklist is in docs/architecture/skep-cluster.md):
//   - the connection is a TLS 1.3 connection the node-to-node listener
//     accepted, and the pin of its key is known;
//   - after each RPC is decoded, and before it reaches the library, the
//     sender is checked (checkSender): protocol version 3, the header's
//     node ID belongs to the key's pin and to a kind of key allowed to make
//     Raft RPCs, the header's address is that node's address, and the node
//     is a server in the receiver's latest Raft configuration;
//   - every read is bounded in size and time (budget, deadlines), and the
//     InstallSnapshot size is bounded;
//   - no metrics.

package raftnet

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"
)

// The RPC type bytes of hashicorp/raft v1.8.0's NetworkTransport.
const (
	rpcAppendEntries uint8 = iota
	rpcRequestVote
	rpcInstallSnapshot
	rpcTimeoutNow
	rpcRequestPreVote
)

// isRaftType reports whether b is one of the library's RPC type bytes.
func isRaftType(b byte) bool { return b <= rpcRequestPreVote }

// connReceiveBufferSize is upstream's read buffer.
const connReceiveBufferSize = 256 * 1024

// serveRaft serves Raft RPCs on an accepted connection whose first byte
// (an RPC type) is still unread in r.
func (t *Transport) serveRaft(conn net.Conn, br *budgetReader, r *bufio.Reader, pin string) {
	w := bufio.NewWriter(conn)
	dec := codec.NewDecoder(r, decodeHandle())
	mp := &codec.MsgpackHandle{}
	mp.TimeNotBuiltin = true // upstream's default (MsgpackUseNewTimeFormat false)
	enc := codec.NewEncoder(w, mp)
	for {
		select {
		case <-t.shutdown:
			return
		default:
		}
		if err := t.handleRaftRPC(conn, br, r, dec, enc, pin); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				t.log.Warn("node listener: raft connection closed", "remote", conn.RemoteAddr().String(), "error", err.Error())
			}
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// handleRaftRPC decodes, checks and dispatches one RPC.
func (t *Transport) handleRaftRPC(conn net.Conn, br *budgetReader, r *bufio.Reader, dec *codec.Decoder, enc *codec.Encoder, pin string) error {
	// Waiting for the next RPC: a pooled connection may idle.
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	br.reset(maxRPCSize)
	rpcType, err := r.ReadByte()
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(rpcTimeout))

	respCh := make(chan raft.RPCResponse, 1)
	rpc := raft.RPC{RespChan: respCh}

	isHeartbeat := false
	var hdr raft.RPCHeader
	switch rpcType {
	case rpcAppendEntries:
		var req raft.AppendEntriesRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}
		rpc.Command = &req
		hdr = req.RPCHeader
		leaderAddr := req.Addr
		if len(leaderAddr) == 0 {
			//lint:ignore SA1019 mirrors upstream's fallback
			leaderAddr = req.Leader
		}
		// Check if this is a heartbeat (upstream's rule, unchanged).
		if req.Term != 0 && leaderAddr != nil &&
			req.PrevLogEntry == 0 && req.PrevLogTerm == 0 &&
			len(req.Entries) == 0 && req.LeaderCommitIndex == 0 {
			isHeartbeat = true
		}
		//lint:ignore SA1019 the deprecated field must agree with the header
		if len(req.Leader) != 0 && string(req.Leader) != string(req.Addr) {
			return errors.New("the deprecated leader field does not match the header")
		}
	case rpcRequestVote:
		var req raft.RequestVoteRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}
		rpc.Command = &req
		hdr = req.RPCHeader
		//lint:ignore SA1019 the deprecated field must agree with the header
		if len(req.Candidate) != 0 && string(req.Candidate) != string(req.Addr) {
			return errors.New("the deprecated candidate field does not match the header")
		}
	case rpcRequestPreVote:
		var req raft.RequestPreVoteRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}
		rpc.Command = &req
		hdr = req.RPCHeader
	case rpcInstallSnapshot:
		var req raft.InstallSnapshotRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}
		if req.Size < 0 || req.Size > maxSnapshotSize {
			return fmt.Errorf("snapshot of %d bytes is over the limit of %d", req.Size, maxSnapshotSize)
		}
		// The older leader field, if set, must agree with the header.
		if len(req.Leader) != 0 && string(req.Leader) != string(req.Addr) {
			return errors.New("the leader field does not match the header")
		}
		rpc.Command = &req
		// The snapshot data follows the request on the connection.
		br.reset(req.Size + connReceiveBufferSize)
		_ = conn.SetReadDeadline(time.Now().Add(snapshotTimeout(req.Size)))
		rpc.Reader = io.LimitReader(r, req.Size)
		hdr = req.RPCHeader
	case rpcTimeoutNow:
		var req raft.TimeoutNowRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}
		rpc.Command = &req
		hdr = req.RPCHeader
	default:
		return fmt.Errorf("unknown rpc type %d", rpcType)
	}

	// Skep's check: who sent this RPC.
	if err := t.checkSender(pin, hdr, isHeartbeat); err != nil {
		t.refused(conn, pin, rpcType, hdr, err)
		return err
	}

	// Check for heartbeat fast-path
	if isHeartbeat {
		t.hbMu.Lock()
		fn := t.hbFn
		t.hbMu.Unlock()
		if fn != nil {
			fn(rpc)
			goto RESP
		}
	}

	// Dispatch the RPC
	select {
	case t.consumeCh <- rpc:
	case <-t.shutdown:
		return raft.ErrTransportShutdown
	}

	// Wait for response
RESP:
	select {
	case resp := <-respCh:
		// Send the error first
		respErr := ""
		if resp.Error != nil {
			respErr = resp.Error.Error()
		}
		_ = conn.SetWriteDeadline(time.Now().Add(rpcTimeout))
		if err := enc.Encode(respErr); err != nil {
			return err
		}
		// Send the response
		if err := enc.Encode(resp.Response); err != nil {
			return err
		}
	case <-t.shutdown:
		return raft.ErrTransportShutdown
	}
	return nil
}
