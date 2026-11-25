package clusterstate

// describe returns what an entry names, for the cluster audit: the node,
// its name and address and the pins, looked up in the state before the
// entry was applied where the entry carries only the node ID, and the
// entry's own time where it has one. It never fails: data that cannot be
// read leaves the fields empty (the machine stalls on such an entry
// before the audit is asked).
func (s state) describe(e Entry) (nodeID, name, address string, pins []string, at int64) {
	fromMember := func(id string) {
		nodeID = id
		if m, ok := s.members[id]; ok {
			name, address = m.Name, m.Address
			pins = memberPins(m)
		} else if p, ok := s.pendings[id]; ok {
			name, address, pins = p.Name, p.Address, []string{p.Pin}
		}
	}
	switch e.Type {
	case TypeClusterInit:
		var d ClusterInit
		if decodeData(e.Data, &d) == nil {
			nodeID, name, address, pins = d.Member.ID, d.Member.Name, d.Member.Address, []string{d.Member.Pin}
		}
	case TypeMemberAddress:
		var d MemberAddress
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
			address = d.Address
		}
	case TypePendingCreate:
		var d PendingCreate
		if decodeData(e.Data, &d) == nil {
			nodeID, name, address, pins = d.NodeID, d.Name, d.Address, []string{d.Pin}
		}
	case TypePendingWithdraw:
		var d PendingWithdraw
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
			name = d.Name
		}
	case TypePendingExpire:
		var d PendingExpire
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
			at = d.At
		}
	case TypeMemberJoin:
		var d MemberJoin
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
			pins, at = []string{d.Pin}, d.At
		}
	case TypeMemberPromoted:
		var d MemberPromoted
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	case TypeMemberRemoving:
		var d MemberRemoving
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	case TypeMemberRemoved:
		var d MemberRemoved
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	}
	return nodeID, name, address, pins, at
}

// memberPins returns a member's pins.
func memberPins(m Member) []string { return []string{m.Pin} }
