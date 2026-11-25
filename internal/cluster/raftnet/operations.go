// Package raftnet carries Raft RPCs and Skep's own node-to-node RPCs over
// mutual TLS 1.3 on port 8444, with the sender's identity checked on every
// RPC (ADR 0013 B1, "Per-RPC identity").
//
// Outbound Raft RPCs use the library's NetworkTransport over a stream layer
// whose dialler accepts only the pin of the node it means to reach.
// Inbound RPCs are served by Skep itself (net_handler.go, adapted from the
// library's net_transport.go, MPL-2.0), because NetworkTransport does not
// hand the connection to the RPC consumer.
//
// With the store package, this is the only package that imports the Raft
// library.
package raftnet

// Kind is what a key is to the node that receives a connection from it.
type Kind int

const (
	// KindNone is a key the node does not accept.
	KindNone Kind = iota
	// KindPending is the key of an admitted host that has not joined:
	// it may call the join operation and nothing else.
	KindPending
	// KindJoining is the key of a member entry in state "joining".
	KindJoining
	// KindMember is the key of a member entry in state "member", or,
	// on a node that is joining, a key from its admission or from the
	// join call's answer.
	KindMember
	// KindRemoving is the key of a member entry in state "removing"
	// (removal step 0). It keeps Raft RPCs and the status RPC while the
	// node is a server in the receiver's latest Raft configuration, and
	// nothing else.
	KindRemoving
)

func (k Kind) String() string {
	switch k {
	case KindPending:
		return "pending"
	case KindJoining:
		return "joining"
	case KindMember:
		return "member"
	case KindRemoving:
		return "removing"
	}
	return "none"
}

// Peer is a node the receiver knows by its key.
type Peer struct {
	NodeID  string
	Name    string
	Address string
	Pin     string
	Kind    Kind
}

// Operation names.
const (
	// OpRaft is every Raft RPC: AppendEntries (with the heartbeat fast
	// path), RequestVote, RequestPreVote, InstallSnapshot, TimeoutNow.
	OpRaft = "raft"
	// OpJoin is the join call (ADR 0013 B1, join command step 5).
	OpJoin = "join"
	// OpStatus is Skep's status RPC: the state machine's applied index,
	// for catch-up and the promotion rule.
	OpStatus = "status"
	// OpAdmit is "admit" passed by a member to the leader.
	OpAdmit = "admit"
	// OpWithdraw is "admit --withdraw" passed by a member to the leader.
	OpWithdraw = "withdraw"
	// OpRemove is "skep-cluster remove" passed by a member to the leader.
	OpRemove = "remove"
	// OpRemoved is the best-effort notice a member sends a removed node
	// (ADR 0013 B1, decision D11 of step 8b-2a).
	OpRemoved = "removed"
	// OpClock answers the node's clock in Unix milliseconds, for the
	// clock-offset health check (D3 of step 8b-2b). It is its own
	// operation, not a field of status, so that a node of an older build
	// answers status as before and its clock reads "not known".
	OpClock = "clock"
)

// Operation says which kinds of key may call an operation.
type Operation struct {
	Name     string
	Pending  bool
	Joining  bool
	Member   bool
	Removing bool
	// RaftServer requires, in addition, that the sender is a server in
	// the receiver's latest Raft configuration (ADR 0013 B1, rule 2).
	RaftServer bool
}

// Operations is every operation on port 8444 and who may call it. The
// tests are driven from this table, and the documentation's table is
// checked against it.
var Operations = []Operation{
	{Name: OpRaft, Joining: true, Member: true, Removing: true, RaftServer: true},
	// A joining node repeats its join call with the same operation ID
	// when the outcome was unknown, so its key keeps the operation
	// after join step 1.
	{Name: OpJoin, Pending: true, Joining: true},
	// The joining node asks every member for its applied index, as every
	// member does, so health on every member (the joiner included) names
	// the member being waited for.
	{Name: OpStatus, Joining: true, Member: true, Removing: true},
	// Every key that may call status may ask for the clock.
	{Name: OpClock, Joining: true, Member: true, Removing: true},
	{Name: OpAdmit, Member: true},
	{Name: OpWithdraw, Member: true},
	{Name: OpRemove, Member: true},
	// On a joining node the admission's and the answer's keys are
	// member keys, so a member can tell a joining node it was removed.
	{Name: OpRemoved, Member: true},
}

// Allowed reports whether a key of this kind may call the operation. An
// unknown operation is never allowed.
func Allowed(op string, k Kind) (Operation, bool) {
	for _, o := range Operations {
		if o.Name != op {
			continue
		}
		switch k {
		case KindPending:
			return o, o.Pending
		case KindJoining:
			return o, o.Joining
		case KindMember:
			return o, o.Member
		case KindRemoving:
			return o, o.Removing
		}
		return o, false
	}
	return Operation{}, false
}

// Trust is what the transport asks about keys. The service implements it
// from the applied membership, the applied-membership file, the admission
// of a joining node and the Raft configuration.
type Trust interface {
	// ByPin returns the node a key belongs to now. A pending key whose
	// expiry time has passed by this node's clock is not returned.
	ByPin(pin string) (Peer, bool)
	// ByAddress returns the node the dialler expects at an address:
	// only joining and member kinds.
	ByAddress(address string) (Peer, bool)
	// RaftServer reports whether the node is a server in the latest Raft
	// configuration, or, on a node that is joining and not yet listed in
	// its own applied membership, whether it is a node of its admission.
	// heartbeat says the RPC is on the heartbeat fast path, where a
	// cached configuration (at most CacheAge old) is used so that the
	// fast path never waits for the Raft main loop.
	RaftServer(nodeID string, heartbeat bool) bool
}
