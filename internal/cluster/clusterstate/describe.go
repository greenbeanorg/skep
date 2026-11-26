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
		if e.Version == 2 {
			var d2 MemberJoinV2
			if decodeData(e.Data, &d2) == nil {
				fromMember(d2.NodeID)
				pins, at = []string{d2.Pin}, d2.At
			}
		} else if decodeData(e.Data, &d) == nil {
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
		if e.Version == 2 {
			var d2 MemberRemovingV2
			if decodeData(e.Data, &d2) == nil {
				fromMember(d2.NodeID)
			}
		} else if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	case TypeMemberRemoved:
		var d MemberRemoved
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	case TypeMemberKeyAdd:
		var d MemberKeyAdd
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
			pins, at = append(pins, d.NextPin), d.At
		}
	case TypeMemberKeyDone:
		var d MemberKeyDone
		if decodeData(e.Data, &d) == nil {
			fromMember(d.NodeID)
		}
	}
	return nodeID, name, address, pins, at
}

// memberPins returns a member's pins.
func memberPins(m Member) []string {
	if m.NextPin != "" {
		return []string{m.Pin, m.NextPin}
	}
	return []string{m.Pin}
}

// describeGuests returns what a member entry says about guests for the
// audit (D7 of step 8d-2), by the state before it: a member.join version
// 2's guests; a member.removing version 2's counts; for an applied
// member.removed, the counts of what it removed. Nothing for any other
// entry, so every earlier record is written as before.
func (s state) describeGuests(e Entry, outcome string) (guests []JoinGuest, guestCount, reservationCount int) {
	switch {
	case e.Type == TypeMemberJoin && e.Version == 2:
		var d MemberJoinV2
		if decodeData(e.Data, &d) == nil {
			guests = d.Guests
		}
	case e.Type == TypeMemberRemoving && e.Version == 2:
		var d MemberRemovingV2
		if decodeData(e.Data, &d) == nil {
			guestCount, reservationCount = d.Guests, d.Reservations
		}
	case e.Type == TypeMemberRemoved && outcome == "applied":
		var d MemberRemoved
		if decodeData(e.Data, &d) == nil {
			guestCount, reservationCount = s.ownsGuests(d.NodeID)
		}
	}
	return guests, guestCount, reservationCount
}
