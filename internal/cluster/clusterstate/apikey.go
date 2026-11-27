package clusterstate

// applyAPIKey applies member.api_key version 1 (step 8e-1, D3): it sets a
// member's API pin, the pin of its skep-api's node-to-node key. The checks
// run in a fixed order over sorted members and pending entries, so every
// node refuses the same entry with the same code and text.
func (s *state) applyAPIKey(e Entry) (change, error) {
	var d MemberAPIKey
	if err := decodeOrStall(e, &d); err != nil {
		return change{}, err
	}
	mem, ok := s.members[d.NodeID]
	if !ok {
		return change{}, refuse(CodeNotFound, "node %s is not a member", d.NodeID)
	}
	if mem.State != StateMember {
		return change{}, refuse(CodeInvalid, "member %s is in state %s, not member: an API key is registered only by a member", mem.Name, mem.State)
	}
	if !ValidPin(d.APIPin) {
		return change{}, refuse(CodeInvalid, "API pin %q is not a key pin", d.APIPin)
	}
	if d.APIPin == mem.APIPin {
		// The pin the member already has: a success that changes nothing
		// (the guest entries' pattern).
		return change{unchanged: true}, nil
	}
	if d.APIPin == mem.Pin || (mem.NextPin != "" && d.APIPin == mem.NextPin) {
		return change{}, refuse(CodeInvalid, "the API pin of %s is its own node key's pin; skep-api's key is a separate key", mem.Name)
	}
	for _, other := range s.sortedMembers() {
		if other.ID == mem.ID {
			continue
		}
		if d.APIPin == other.Pin || (other.NextPin != "" && d.APIPin == other.NextPin) || d.APIPin == other.APIPin {
			return change{}, refuse(CodeInvalid, "the pin %s belongs to member %s", d.APIPin, other.Name)
		}
	}
	for _, p := range s.sortedPendings() {
		if d.APIPin == p.Pin {
			return change{}, refuse(CodeInvalid, "the pin %s belongs to the pending entry of %s", d.APIPin, p.Name)
		}
	}
	mem.APIPin = d.APIPin
	s.members[d.NodeID] = mem
	return change{membership: true, events: []Event{{Kind: "member API key registered", NodeID: mem.ID, Name: mem.Name}}}, nil
}

// GuestByID returns one guest of the inventory by its ID (step 8e-1, D12),
// without its computed flag.
func (m *Machine) GuestByID(id string) (Guest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.st.guests[id]
	return g, ok
}
