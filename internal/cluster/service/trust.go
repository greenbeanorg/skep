package service

import (
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
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
//     admission's and the answer's nodes instead.
func (s *Service) acceptedNow() accepted {
	if s.machine == nil {
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
			a.peers = append(a.peers, raftnet.Peer{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin, Kind: kindOf(m.State)})
		}
	} else {
		a.source = "state machine"
		for _, m := range view.Members {
			a.peers = append(a.peers, raftnet.Peer{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin, Kind: kindOf(m.State)})
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
	if js := s.joinState(); js != nil && !listsNode(a.peers, s.cfg.NodeID) {
		a = accepted{admission: true, source: "admission"}
		for _, n := range js.Peers(s.cfg.NodeID) {
			a.peers = append(a.peers, raftnet.Peer{NodeID: n.NodeID, Name: n.Name, Address: n.Address, Pin: n.Pin, Kind: raftnet.KindMember})
		}
	}
	return a
}

func listsNode(peers []raftnet.Peer, id string) bool {
	for _, p := range peers {
		if p.NodeID == id && p.Kind != raftnet.KindPending {
			return true
		}
	}
	return false
}

// ByPin implements raftnet.Trust.
func (s *Service) ByPin(pin string) (raftnet.Peer, bool) {
	if pin == "" {
		return raftnet.Peer{}, false
	}
	for _, p := range s.acceptedNow().peers {
		if p.Pin == pin {
			return p, true
		}
	}
	return raftnet.Peer{}, false
}

// ByAddress implements raftnet.Trust: only joining and member keys.
func (s *Service) ByAddress(address string) (raftnet.Peer, bool) {
	if address == "" {
		return raftnet.Peer{}, false
	}
	var found raftnet.Peer
	n := 0
	for _, p := range s.acceptedNow().peers {
		if p.Address == address && (p.Kind == raftnet.KindJoining || p.Kind == raftnet.KindMember) {
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
