package raftnet

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
)

const (
	idServer  = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"
	idMember  = "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22"
	idJoining = "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33"
	idPending = "3e8b6d85-ac3b-4f8b-8a3e-6c4a9c3d2f44"
	idOther   = "4f9c7e96-bd4c-4a9c-9b4f-7d5bad4e3a55"
)

// fakeTrust is a membership: keys by pin, and the Raft servers.
type fakeTrust struct {
	mu      sync.Mutex
	peers   map[string]Peer // by pin
	servers map[string]bool
}

func (f *fakeTrust) ByPin(pin string) (Peer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.peers[pin]
	return p, ok
}

func (f *fakeTrust) ByAddress(addr string) (Peer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.peers {
		if p.Address == addr && (p.Kind == KindMember || p.Kind == KindJoining) {
			return p, true
		}
	}
	return Peer{}, false
}

func (f *fakeTrust) RaftServer(id string, _ bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servers[id]
}

func (f *fakeTrust) remove(pin string) {
	f.mu.Lock()
	delete(f.peers, pin)
	f.mu.Unlock()
}

type echoPeer struct{}

func (echoPeer) HandlePeer(_ context.Context, op string, from Peer, _ json.RawMessage) (any, error) {
	return map[string]string{"op": op, "from": from.NodeID}, nil
}

func newKey(t *testing.T, id string) *nodekey.Key {
	t.Helper()
	k, err := nodekey.Create(t.TempDir(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// rig is a receiving node: its listener and transport, and a consumer
// that answers every Raft RPC it gets.
type rig struct {
	addr    string
	server  *nodekey.Key
	trust   *fakeTrust
	keys    map[string]*nodekey.Key // by node ID
	got     chan raft.RPC
	trans   *Transport
	ln      *peerlistener.Listener
	address map[string]string // node ID -> claimed address
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{server: newKey(t, idServer), keys: map[string]*nodekey.Key{}, got: make(chan raft.RPC, 16),
		address: map[string]string{idMember: "127.0.0.1:18001", idJoining: "127.0.0.1:18002", idPending: "127.0.0.1:18003"}}
	r.trust = &fakeTrust{peers: map[string]Peer{}, servers: map[string]bool{idMember: true, idJoining: true}}
	for id, kind := range map[string]Kind{idMember: KindMember, idJoining: KindJoining, idPending: KindPending} {
		k := newKey(t, id)
		r.keys[id] = k
		r.trust.peers[k.Pin()] = Peer{NodeID: id, Address: r.address[id], Pin: k.Pin(), Kind: kind}
	}
	r.keys[idOther] = newKey(t, idOther) // a key nobody admitted
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	ln.Close()
	r.trans, err = New(Options{LocalAddress: r.addr, Certificate: r.server.Certificate, Trust: r.trust, Peer: echoPeer{}})
	if err != nil {
		t.Fatal(err)
	}
	r.ln, err = peerlistener.Listen(peerlistener.Options{Address: netip.MustParseAddrPort(r.addr), Certificate: r.server.Certificate,
		Accept: func(pin string) bool { _, ok := r.trust.ByPin(pin); return ok }, Handle: r.trans.Serve})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for rpc := range r.trans.Consumer() {
			r.got <- rpc
			switch rpc.Command.(type) {
			case *raft.AppendEntriesRequest:
				rpc.Respond(&raft.AppendEntriesResponse{}, nil)
			case *raft.RequestVoteRequest:
				rpc.Respond(&raft.RequestVoteResponse{}, nil)
			case *raft.RequestPreVoteRequest:
				rpc.Respond(&raft.RequestPreVoteResponse{}, nil)
			case *raft.TimeoutNowRequest:
				rpc.Respond(&raft.TimeoutNowResponse{}, nil)
			case *raft.InstallSnapshotRequest:
				rpc.Respond(&raft.InstallSnapshotResponse{}, nil)
			}
		}
	}()
	t.Cleanup(func() { r.ln.Close(); r.trans.Close() })
	return r
}

func header(id, addr string) raft.RPCHeader {
	return raft.RPCHeader{ProtocolVersion: 3, ID: []byte(id), Addr: []byte(addr)}
}

// rpcOf builds one RPC of each type with the given header.
func rpcOf(typ uint8, h raft.RPCHeader) any {
	switch typ {
	case rpcAppendEntries:
		return &raft.AppendEntriesRequest{RPCHeader: h, Term: 1, PrevLogEntry: 1, LeaderCommitIndex: 1}
	case rpcRequestVote:
		return &raft.RequestVoteRequest{RPCHeader: h, Term: 1}
	case rpcRequestPreVote:
		return &raft.RequestPreVoteRequest{RPCHeader: h, Term: 1}
	case rpcTimeoutNow:
		return &raft.TimeoutNowRequest{RPCHeader: h}
	case rpcInstallSnapshot:
		return &raft.InstallSnapshotRequest{RPCHeader: h, Term: 1, Size: 0}
	}
	return nil
}

func heartbeatOf(h raft.RPCHeader) any {
	return &raft.AppendEntriesRequest{RPCHeader: h, Term: 1}
}

// sendRaft sends one Raft RPC the way the library's NetworkTransport does
// and reports whether the receiver answered.
func sendRaft(cert *tls.Certificate, addr, serverPin string, local *net.TCPAddr, typ uint8, req any) error {
	d := net.Dialer{Timeout: 5 * time.Second, LocalAddr: local}
	raw, err := d.Dial("tcp", addr)
	if err != nil {
		return err
	}
	tc := tls.Client(raw, ClientTLSConfig(serverPin, func() *tls.Certificate { return cert }))
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		return &HandshakeError{Address: addr, Err: err}
	}
	if err := confirmAccepted(tc, 5*time.Second); err != nil {
		return &HandshakeError{Address: addr, Err: err}
	}
	w := bufio.NewWriter(tc)
	mp := &codec.MsgpackHandle{}
	mp.TimeNotBuiltin = true
	enc := codec.NewEncoder(w, mp)
	if err := w.WriteByte(typ); err != nil {
		return err
	}
	if err := enc.Encode(req); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	dec := codec.NewDecoder(bufio.NewReader(tc), &codec.MsgpackHandle{})
	var respErr string
	if err := dec.Decode(&respErr); err != nil {
		return err
	}
	if respErr != "" {
		return errors.New(respErr)
	}
	return nil
}

var allRaftTypes = []uint8{rpcAppendEntries, rpcRequestVote, rpcRequestPreVote, rpcTimeoutNow, rpcInstallSnapshot}

// The prototype's identity tests, against pins (ADR 0013 B1, Tests).
func TestPerRPCIdentity(t *testing.T) {
	r := newRig(t)
	member := r.keys[idMember].Certificate()
	mAddr := r.address[idMember]
	cases := []struct {
		name string
		cert *tls.Certificate
		hdr  raft.RPCHeader
		ok   bool
	}{
		{"member with its own header", member, header(idMember, mAddr), true},
		{"another key (nobody admitted it)", r.keys[idOther].Certificate(), header(idMember, mAddr), false},
		{"a valid key of another node claiming the member", r.keys[idJoining].Certificate(), header(idMember, mAddr), false},
		{"a non-member: the pending key", r.keys[idPending].Certificate(), header(idPending, r.address[idPending]), false},
		{"header claiming another node", member, header(idJoining, r.address[idJoining]), false},
		{"header claiming another address", member, header(idMember, "127.0.0.1:19999"), false},
		{"header without an ID", member, header("", mAddr), false},
		{"protocol version 2", member, raft.RPCHeader{ProtocolVersion: 2, ID: []byte(idMember), Addr: []byte(mAddr)}, false},
	}
	for _, c := range cases {
		for _, typ := range allRaftTypes {
			err := sendRaft(c.cert, r.addr, r.server.Pin(), nil, typ, rpcOf(typ, c.hdr))
			if (err == nil) != c.ok {
				t.Errorf("%s, rpc type %d: %v", c.name, typ, err)
			}
		}
		// The heartbeat fast path is checked too.
		if err := sendRaft(c.cert, r.addr, r.server.Pin(), nil, rpcAppendEntries, heartbeatOf(c.hdr)); (err == nil) != c.ok {
			t.Errorf("%s, heartbeat: %v", c.name, err)
		}
	}
	// Keys nobody admitted fail in the handshake, before any RPC.
	err := sendRaft(r.keys[idOther].Certificate(), r.addr, r.server.Pin(), nil, rpcAppendEntries, rpcOf(rpcAppendEntries, header(idOther, "x")))
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Errorf("unadmitted key: %v, want a refusal in the handshake", err)
	}
}

// A member that is not a server in the Raft configuration is refused (rule
// 2), and a member connecting from another address than its entry's is
// accepted: the source address is not compared.
func TestRaftServerRuleAndSourceAddress(t *testing.T) {
	r := newRig(t)
	member := r.keys[idMember].Certificate()
	hdr := header(idMember, r.address[idMember])
	if err := sendRaft(member, r.addr, r.server.Pin(), &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}, rpcAppendEntries, rpcOf(rpcAppendEntries, hdr)); err != nil {
		t.Fatalf("member from a second loopback address: %v", err)
	}
	r.trust.mu.Lock()
	r.trust.servers[idMember] = false
	r.trust.mu.Unlock()
	if err := sendRaft(member, r.addr, r.server.Pin(), nil, rpcAppendEntries, rpcOf(rpcAppendEntries, hdr)); err == nil {
		t.Fatal("a member that is not a Raft server was accepted")
	}
}

func TestTLS12AndNoCertificateRefused(t *testing.T) {
	r := newRig(t)
	for _, conf := range []*tls.Config{
		{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*r.keys[idMember].Certificate()}}, // test client
		{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}, // test client without a certificate
	} {
		c, err := tls.Dial("tcp", r.addr, conf)
		if err == nil {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = c.Read(make([]byte, 1))
			c.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "tls") {
			t.Errorf("handshake max %x: %v, want a TLS refusal", conf.MaxVersion, err)
		}
	}
}

// The dialler accepts only the pin of the node it means to reach: an
// impostor listening at a member's address with another key is refused,
// and an address no member has is not dialled.
func TestDiallerAcceptsOnlyTheExpectedPin(t *testing.T) {
	r := newRig(t)
	impostor := newKey(t, idOther)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	l, err := peerlistener.Listen(peerlistener.Options{Address: netip.MustParseAddrPort(addr), Certificate: impostor.Certificate,
		Accept: func(string) bool { return true }, Handle: func(c *tls.Conn, _ string) { c.Write([]byte{acceptedMarker}); c.Read(make([]byte, 1)) }})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// The rig's trust says the member is at addr, with the member's pin.
	r.trust.mu.Lock()
	p := r.trust.peers[r.keys[idMember].Pin()]
	p.Address = addr
	r.trust.peers[p.Pin] = p
	r.trust.mu.Unlock()
	_, err = streamLayer{r.trans}.Dial(raft.ServerAddress(addr), 5*time.Second)
	var he *HandshakeError
	if !errors.As(err, &he) || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("impostor at a member's address: %v", err)
	}
	if _, err := (streamLayer{r.trans}).Dial("127.0.0.1:1", time.Second); err == nil || !strings.Contains(err.Error(), "no member") {
		t.Fatalf("unknown address: %v", err)
	}
}

// Skep's own operations are open exactly to the kinds of key the table
// names; the tests are driven from Operations.
func TestPeerOperationsFollowTheTable(t *testing.T) {
	r := newRig(t)
	for _, op := range Operations {
		if op.Name == OpRaft {
			continue
		}
		for _, k := range []struct {
			id   string
			kind Kind
		}{{idMember, KindMember}, {idJoining, KindJoining}, {idPending, KindPending}} {
			c := Caller{NodeID: k.id, Certificate: r.keys[k.id].Certificate}
			var out map[string]string
			err := c.Call(context.Background(), r.addr, r.server.Pin(), op.Name, struct{}{}, &out)
			_, want := Allowed(op.Name, k.kind)
			if want && (err != nil || out["op"] != op.Name || out["from"] != k.id) {
				t.Errorf("%s by a %s key: %v %v", op.Name, k.kind, err, out)
			}
			var pe *PeerError
			if !want && (!errors.As(err, &pe) || pe.Code != CodeForbidden) {
				t.Errorf("%s by a %s key: %v, want forbidden", op.Name, k.kind, err)
			}
		}
	}
	// Raft RPCs follow the table too.
	for _, k := range []struct {
		id   string
		kind Kind
	}{{idMember, KindMember}, {idJoining, KindJoining}, {idPending, KindPending}} {
		err := sendRaft(r.keys[k.id].Certificate(), r.addr, r.server.Pin(), nil, rpcAppendEntries, rpcOf(rpcAppendEntries, header(k.id, r.address[k.id])))
		if _, want := Allowed(OpRaft, k.kind); (err == nil) != want {
			t.Errorf("raft by a %s key: %v", k.kind, err)
		}
	}
	// A request claiming another node than its key's is refused.
	c := Caller{NodeID: idMember, Certificate: r.keys[idPending].Certificate}
	var pe *PeerError
	if err := c.Call(context.Background(), r.addr, r.server.Pin(), OpJoin, struct{}{}, nil); !errors.As(err, &pe) || pe.Code != CodeForbidden {
		t.Errorf("join claiming another node: %v", err)
	}
	// An unknown operation is never allowed.
	if _, ok := Allowed("shell", KindMember); ok {
		t.Error("an unknown operation is allowed")
	}
}

// The pinned table: a change to who may call what fails here.
func TestOperationTable(t *testing.T) {
	want := map[string][3]bool{ // pending, joining, member
		OpRaft:     {false, true, true},
		OpJoin:     {true, true, false},
		OpStatus:   {false, true, true},
		OpAdmit:    {false, false, true},
		OpWithdraw: {false, false, true},
	}
	if len(Operations) != len(want) {
		t.Fatalf("%d operations, want %d", len(Operations), len(want))
	}
	for _, op := range Operations {
		if w, ok := want[op.Name]; !ok || w != [3]bool{op.Pending, op.Joining, op.Member} || op.RaftServer != (op.Name == OpRaft) {
			t.Errorf("operation %+v is not in the reviewed table", op)
		}
	}
}

// A key that stops being accepted loses its open connections and every
// later RPC on a connection it still holds.
func TestRemovedKeyIsRefusedPerRPC(t *testing.T) {
	r := newRig(t)
	member := r.keys[idMember].Certificate()
	hdr := header(idMember, r.address[idMember])
	if err := sendRaft(member, r.addr, r.server.Pin(), nil, rpcAppendEntries, rpcOf(rpcAppendEntries, hdr)); err != nil {
		t.Fatal(err)
	}
	r.trust.remove(r.keys[idMember].Pin())
	if err := sendRaft(member, r.addr, r.server.Pin(), nil, rpcAppendEntries, rpcOf(rpcAppendEntries, hdr)); err == nil {
		t.Fatal("a removed key was accepted")
	}
}

// Oversized and malformed input closes the connection.
func TestSizeLimits(t *testing.T) {
	r := newRig(t)
	member := r.keys[idMember].Certificate()
	hdr := header(idMember, r.address[idMember])
	big := &raft.InstallSnapshotRequest{RPCHeader: hdr, Term: 1, Size: maxSnapshotSize + 1}
	if err := sendRaft(member, r.addr, r.server.Pin(), nil, rpcInstallSnapshot, big); err == nil {
		t.Fatal("a snapshot over the limit was accepted")
	}
	if err := writeFrame(&strings.Builder{}, strings.Repeat("x", maxPeerMessage+1)); err == nil {
		t.Fatal("an oversized frame was written")
	}
	if err := readFrame(strings.NewReader("\xff\xff\xff\xff"), new(map[string]any)); err == nil {
		t.Fatal("an oversized frame was read")
	}
}
