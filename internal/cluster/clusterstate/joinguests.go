package clusterstate

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Limits of a joining host's guest list (step 8d-2, D4). The root command
// checks them before anything changes, the stage before it moves the
// one-node state aside, and the leader before it proposes; the count is
// checked again at apply. They keep the member.join entry within one
// entry's limit (MaxImportBytes, 192 KiB) and the join call within one
// Skep RPC (256 KiB, envelope included).
const (
	// MaxJoinGuests is the most guests a host may join with.
	MaxJoinGuests = 1000
	// MaxJoinGuestsBytes bounds the JSON encoding (encoding/json) of the
	// guests array.
	MaxJoinGuestsBytes = 160 << 10
)

// CheckJoinGuestLimits checks D4's two limits.
func CheckJoinGuestLimits(guests []JoinGuest) error {
	if len(guests) > MaxJoinGuests {
		return refuse(CodeInvalid, "the host has %d guests; at most %d may join with it", len(guests), MaxJoinGuests)
	}
	b, err := json.Marshal(guests)
	if err != nil {
		return refuse(CodeInvalid, "the guest list cannot be encoded: %v", err)
	}
	if len(b) > MaxJoinGuestsBytes {
		return refuse(CodeInvalid, "the guest list is %d bytes as JSON; at most %d may join", len(b), MaxJoinGuestsBytes)
	}
	return nil
}

// CheckJoinGuests checks a non-empty join list by D3's rules a to c: 1 to
// MaxJoinGuests guests, each with a lower-case UUID, the kind vm or
// container and a valid name, no ID and no kind and name twice. The first
// guest that breaks a rule, in list order, is named.
func CheckJoinGuests(guests []JoinGuest) error {
	if len(guests) < 1 || len(guests) > MaxJoinGuests {
		return refuse(CodeInvalid, "a join's guest list holds 1 to %d guests, not %d", MaxJoinGuests, len(guests))
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, g := range guests {
		switch {
		case !ValidNodeID(g.ID):
			return refuse(CodeInvalid, "the guest ID %q is not a lower-case UUID", g.ID)
		case !ValidGuestKind(g.Kind):
			return refuse(CodeInvalid, "the guest %s has the kind %q, not vm or container", g.ID, g.Kind)
		case !ValidGuestName(g.Name):
			return refuse(CodeInvalid, "the guest %s has a name the cluster does not accept (1 to 255 bytes of UTF-8, no control character)", g.ID)
		case ids[g.ID]:
			return refuse(CodeInvalid, "the guest ID %s is in the list twice", g.ID)
		case names[resKey(g.Kind, g.Name)]:
			return refuse(CodeInvalid, "the %s name %q is in the list twice", g.Kind, g.Name)
		}
		ids[g.ID], names[resKey(g.Kind, g.Name)] = true, true
	}
	return nil
}

// checkJoinGuests is D3's rules a to f against this state, in that order,
// each a refusal that is the same on every node. It reads the maps only
// through lookups and guestNamed (which picks the guest added first), so
// the result does not depend on map order.
func (s state) checkJoinGuests(guests []JoinGuest) error {
	if err := CheckJoinGuests(guests); err != nil {
		return err
	}
	for _, g := range guests {
		if _, ok := s.guests[g.ID]; ok {
			return refuse(CodeInvalid, "the guest ID %s is already in the cluster's inventory", g.ID)
		}
	}
	// guest.reserve's words: a reservation first, then a guest.
	for _, g := range guests {
		if r, ok := s.reservations[resKey(g.Kind, g.Name)]; ok {
			return refuse(CodeNameTaken, "the %s name %q is reserved by node %s", g.Kind, g.Name, s.holderName(r.Owner))
		}
		if h, ok := s.guestNamed(g.Kind, g.Name); ok {
			return refuse(CodeNameTaken, "the %s name %q belongs to a guest of node %s", g.Kind, g.Name, s.holderName(h.Owner))
		}
	}
	if n := s.guestCount() + len(guests); n > MaxGuests {
		return refuse(CodeTooManyGuests, "with the %d guests of the joining host the cluster would hold %d guests and reservations, more than %d", len(guests), n, MaxGuests)
	}
	return nil
}

// JoinGuestsRefusal is what applying a member.join version 2 with this
// list would refuse for the guests in the machine's state now (D16: the
// leader's check before it proposes, with the apply rule's text); nil when
// the guests would be accepted.
func (m *Machine) JoinGuestsRefusal(guests []JoinGuest) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.st.checkJoinGuests(guests)
}

// addJoinGuests adds a joining node's guests (D3): owner the node, no key,
// no task ID, added at index and at the entry's time.
func (s *state) addJoinGuests(nodeID string, guests []JoinGuest, index uint64, at int64) {
	next := maps.Clone(s.guests)
	if next == nil {
		next = map[string]Guest{}
	}
	for _, g := range guests {
		next[g.ID] = Guest{ID: g.ID, Kind: g.Kind, Name: g.Name, Owner: nodeID, AddedIndex: index, At: at}
	}
	s.guests, s.guestsIndex = next, index
}

// forgetOwner removes every guest and reservation a node owns (member.removed,
// D6) and returns how many of each it removed. The guests index becomes
// index when it removed any.
func (s *state) forgetOwner(nodeID string, index uint64) (guests, reservations int) {
	g, r := maps.Clone(s.guests), maps.Clone(s.reservations)
	for _, id := range slices.Sorted(maps.Keys(g)) {
		if g[id].Owner == nodeID {
			delete(g, id)
			guests++
		}
	}
	for _, k := range slices.Sorted(maps.Keys(r)) {
		if r[k].Owner == nodeID {
			delete(r, k)
			reservations++
		}
	}
	if guests+reservations > 0 {
		s.guests, s.reservations, s.guestsIndex = g, r, index
	}
	return guests, reservations
}

// GuestsOwnedBy counts the guests and the reservations a node owns in the
// machine's state now (D22: the leader's check of a removal).
func (m *Machine) GuestsOwnedBy(nodeID string) (guests, reservations int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.st.ownsGuests(nodeID)
}

// refuseRemovingV2 is D5's rule for member.removing version 2, in place of
// version 1's "owns a guest or a reservation".
func (s state) refuseRemovingV2(mem Member, d MemberRemovingV2) error {
	g, r := s.ownsGuests(mem.ID)
	if g != d.Guests || r != d.Reservations {
		return fmt.Errorf("member %s owns %d guests and %d name reservations, not the %d and %d this removal forgets", mem.Name, g, r, d.Guests, d.Reservations)
	}
	return nil
}
