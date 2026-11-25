package service

import (
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// accepted is the set of keys the node accepts at one moment (ADR 0013 B1,
// "What the listener accepts", and amendment 1, point 5).
type accepted struct {
	peers []raftnet.Peer
	// admission is true on a joining node that has not yet applied a
	// membership listing itself: it accepts the nodes of its admission
	// and of the join call's answer, and no Raft configuration is
	// consulted.
	admission bool
	// source says where the set came from, for status and tests:
	// "state machine", "membership file", "admission" or "none".
	source string
}

func kindOf(state string) raftnet.Kind {
	switch state {
	case clusterstate.StateJoining:
		return raftnet.KindJoining
	case clusterstate.StateMember:
		return raftnet.KindMember
	case clusterstate.StateRemoving:
		return raftnet.KindRemoving
	}
	return raftnet.KindNone
}

// acceptedNow computes the accepted keys:
//   - a stalled state machine accepts nobody;
//   - until the machine has reached the applied-membership file's index,
//     the file's members, and no pending entry;
//   - from then on the machine's members and its pending entries whose
//     expiry time has not passed by this node's clock;
//   - on a joining node whose accepted membership does not list it, the
//     join call's answer's nodes once one is accepted, the admission's
//     before, and none at all in phase "refused" (decision D10 of 8b-2a);
//   - a removed node accepts nobody.
func (s *Service) acceptedNow() accepted {
	if s.machine == nil || s.isRemoved() {
		return accepted{source: "none"}
	}
	view := s.machine.View()
	if view.Stalled != nil {
		return accepted{source: "none"}
	}
	var a accepted
	file := s.writer.Current()
	if file != nil && view.AppliedIndex < file.Index {
		a.source = "membership file"
		for _, m := range file.Members {
			a.peers = append(a.peers, raftnet.Peer{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin, NextPin: m.NextPin, Kind: kindOf(m.State)})
		}
	} else {
		a.source = "state machine"
		for _, m := range view.Members {
			a.peers = append(a.peers, raftnet.Peer{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin, NextPin: m.NextPin, Kind: kindOf(m.State)})
		}
		now := s.now().Unix()
		for _, p := range view.Pendings {
			// A listener may refuse a pending key earlier, by its own
			// clock; it never accepts one after the expiry entry.
			if now < p.ExpiresAt {
				a.peers = append(a.peers, raftnet.Peer{NodeID: p.NodeID, Name: p.Name, Address: p.Address, Pin: p.Pin, Kind: raftnet.KindPending})
			}
		}
	}
	if js := s.joinState(); js != nil && !(a.source == "state machine" && s.listsSelf(view.Members, js)) {
		if js.Phase == joinstate.PhaseRefused {
			return accepted{admission: true, source: "none (the join was refused)"}
		}
		a = accepted{admission: true, source: "admission"}
		if len(js.Answer) > 0 {
			a.source = "join answer"
		}
		for _, n := range js.Peers(s.cfg.NodeID) {
			a.peers = append(a.peers, raftnet.Peer{NodeID: n.NodeID, Name: n.Name, Address: n.Address, Pin: n.Pin, Kind: raftnet.KindMember})
		}
	}
	return a
}

// listsSelf reports whether the state machine's members list this node
// for this join (F1 of the 8b-2b review): a member entry for this node
// whose join_op is this join's member.join operation ID and whose pin or
// next pin is the current key. A joining node that was a member of the
// same cluster before (removed, abandoned or wiped, joined again with its
// node ID, possibly with the same key) meets its older member entries in
// the log's history; that membership is the cluster's past (other
// members' pins in it may have been rotated or wiped since), so the node
// goes on accepting its admission's or answer's nodes until it applies
// this join's entry. While the machine is behind the applied-membership
// file, the node is not listed (the file has no join_op).
func (s *Service) listsSelf(members []clusterstate.Member, js *joinstate.State) bool {
	if s.key == nil || js == nil || js.OpID == "" {
		return false
	}
	pin, op := s.key.Pin(), joinOpID(s.cfg.NodeID, js.OpID)
	for _, m := range members {
		if m.ID == s.cfg.NodeID && m.JoinOp == op && (m.Pin == pin || (m.NextPin != "" && m.NextPin == pin)) {
			return true
		}
	}
	return false
}

func listsNode(peers []raftnet.Peer, id string) bool {
	for _, p := range peers {
		if p.NodeID == id && p.Kind != raftnet.KindPending {
			return true
		}
	}
	return false
}

// ByPin implements raftnet.Trust. Either pin of a rotating member is that
// member (D1 of step 8b-2b); the per-RPC check is otherwise unchanged.
func (s *Service) ByPin(pin string) (raftnet.Peer, bool) {
	if pin == "" {
		return raftnet.Peer{}, false
	}
	for _, p := range s.acceptedNow().peers {
		if p.Pin == pin || (p.NextPin != "" && p.NextPin == pin) {
			return p, true
		}
	}
	return raftnet.Peer{}, false
}

// SawPin implements raftnet.PinObserver: which pin a node's last
// successful connection to or from this node used.
func (s *Service) SawPin(nodeID, pin string) {
	if nodeID == "" || pin == "" {
		return
	}
	s.mu.Lock()
	s.seen[nodeID] = pinSeen{pin: pin, at: s.now()}
	s.mu.Unlock()
}

// sawPinOf records the pin of an accepted inbound connection.
func (s *Service) sawPinOf(pin string) {
	if p, ok := s.ByPin(pin); ok && p.Kind != raftnet.KindPending {
		s.SawPin(p.NodeID, pin)
	}
}

// ByAddress implements raftnet.Trust: joining, member and removing keys
// (the leader replicates to a removing member until RemoveServer).
func (s *Service) ByAddress(address string) (raftnet.Peer, bool) {
	if address == "" {
		return raftnet.Peer{}, false
	}
	var found raftnet.Peer
	n := 0
	for _, p := range s.acceptedNow().peers {
		if p.Address == address && (p.Kind == raftnet.KindJoining || p.Kind == raftnet.KindMember || p.Kind == raftnet.KindRemoving) {
			found = p
			n++
		}
	}
	// Addresses are unique (checked at apply and in an admission); two
	// nodes at one address would be ambiguous, so nobody is dialled.
	return found, n == 1
}

// RaftServer implements raftnet.Trust.
func (s *Service) RaftServer(nodeID string, heartbeat bool) bool {
	a := s.acceptedNow()
	if a.admission {
		return listsNode(a.peers, nodeID)
	}
	st := s.getStore()
	if st == nil {
		return false
	}
	return st.IsServer(nodeID, heartbeat, raftnet.CacheAge)
}

// acceptPin is the listener's handshake check.
func (s *Service) acceptPin(pin string) bool {
	_, ok := s.ByPin(pin)
	return ok
}

// cannotSay is the listener's "cannot say" rule (D2 of step 8b-2b): this
// node cannot say whether it would accept a key it does not accept now
// while its state machine is stopped, while the machine has not reached
// the applied-membership file's index, or while it is a joining node
// whose join was refused. A key over its connection limit gets the same
// answer from the listener itself.
func (s *Service) cannotSay() bool {
	if s.machine == nil || s.isRemoved() {
		return false
	}
	view := s.machine.View()
	if view.Stalled != nil {
		return true
	}
	if file := s.writer.Current(); file != nil && view.AppliedIndex < file.Index {
		return true
	}
	if js := s.joinState(); js != nil && js.Phase == joinstate.PhaseRefused {
		return true
	}
	return false
}

// connGroup is the listener's per-node limit (D5 of step 8b-2b): a member,
// joining or removing key counts by its node ID (both pins of a rotating
// node are one node), with MaxPerNode; a pending key by itself, with
// MaxPerPending.
func (s *Service) connGroup(pin string) (string, int) {
	p, ok := s.ByPin(pin)
	if !ok {
		return "key:" + pin, peerlistener.MaxPerPending
	}
	if p.Kind == raftnet.KindPending {
		return "pending:" + pin, peerlistener.MaxPerPending
	}
	return "node:" + p.NodeID, peerlistener.MaxPerNode
}
