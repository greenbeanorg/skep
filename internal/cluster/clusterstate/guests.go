package clusterstate

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"unicode/utf8"
)

// Guest entry types (step 8d-1, D10; ADR 0013 amendment 6). The guest
// inventory says which node owns which VM and jail, by ID, kind and name;
// it holds no configuration. A reservation holds a name for a create or a
// clone between the API's request and the guest's ID.
const (
	TypeGuestReserve = "guest.reserve"
	TypeGuestRelease = "guest.release"
	TypeGuestAdd     = "guest.add"
	TypeGuestRemove  = "guest.remove"
)

// GuestTypes are the guest entry types, the only ones guests-write
// accepts.
var GuestTypes = []string{TypeGuestReserve, TypeGuestRelease, TypeGuestAdd, TypeGuestRemove}

// IsGuestType reports whether typ is a guest entry type.
func IsGuestType(typ string) bool { return slices.Contains(GuestTypes, typ) }

// Guest kinds.
const (
	KindVM        = "vm"
	KindContainer = "container"
)

// MaxGuests bounds guests and reservations together (D12).
const MaxGuests = 10000

// Refusal codes of guest entries (D18). CodeInvalid is the users'.
const (
	CodeNameTaken      = "name_taken"
	CodeTooManyGuests  = "too_many_guests"
	CodeOwnerNotMember = "owner_not_member"
	CodeNotOwner       = "not_owner"
)

// guestKeyRE is a reservation's key: an idempotency key's shape.
var guestKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// GuestReserve is the data of guest.reserve, version 1. Owner is a node
// ID, set by skep-cluster, never by the caller (D23). At is the proposing
// leader's clock in Unix seconds.
type GuestReserve struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Key   string `json:"key"`
	Owner string `json:"owner"`
	At    int64  `json:"at"`
}

// GuestRelease is the data of guest.release, version 1.
type GuestRelease struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Key   string `json:"key"`
	Owner string `json:"owner"`
}

// GuestAdd is the data of guest.add, version 1.
type GuestAdd struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
	Owner  string `json:"owner"`
	At     int64  `json:"at"`
}

// GuestRemove is the data of guest.remove, version 1.
type GuestRemove struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
}

// Guest is one guest of the inventory (D11). Flagged is computed when the
// guests are read (D16), never stored.
type Guest struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Key        string `json:"key,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	AddedIndex uint64 `json:"added_index"`
	At         int64  `json:"at"`
	Flagged    bool   `json:"flagged,omitempty"`
}

// Reservation is a name held for a create or clone (D11).
type Reservation struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Key   string `json:"key"`
	At    int64  `json:"at"`
	Index uint64 `json:"index"`
}

// Guests is the inventory as one read sees it.
type Guests struct {
	ClusterID string
	// Index is the index of the last entry that changed the guests or
	// the reservations.
	Index        uint64
	Guests       []Guest
	Reservations []Reservation
}

func resKey(kind, name string) string { return kind + "\x00" + name }

// ValidGuestKind reports whether kind is vm or container.
func ValidGuestKind(kind string) bool { return kind == KindVM || kind == KindContainer }

// ValidGuestName is D12's name rule, by bytes only (D21): valid UTF-8 of 1
// to 255 bytes, no byte below 0x20 and no 0x7f.
func ValidGuestName(name string) bool {
	if len(name) < 1 || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	return true
}

// ValidGuestKey reports whether key is a valid reservation key.
func ValidGuestKey(key string) bool { return guestKeyRE.MatchString(key) }

func (s state) holderName(nodeID string) string {
	if m, ok := s.members[nodeID]; ok {
		return m.Name
	}
	return nodeID
}

func (s state) guestCount() int { return len(s.guests) + len(s.reservations) }

// guestNamed returns a guest of kind and name, the one added first.
func (s state) guestNamed(kind, name string) (Guest, bool) {
	var best Guest
	found := false
	for _, g := range s.guests {
		if g.Kind == kind && g.Name == name && (!found || g.AddedIndex < best.AddedIndex) {
			best, found = g, true
		}
	}
	return best, found
}

func (s state) checkOwner(owner string) error {
	if m, ok := s.members[owner]; !ok || m.State != StateMember {
		return refuse(CodeOwnerNotMember, "the node %s is not a member in state member", owner)
	}
	return nil
}

// ownsGuests says what a node owns, for member.removing (D17).
func (s state) ownsGuests(nodeID string) (guests, reservations int) {
	for _, g := range s.guests {
		if g.Owner == nodeID {
			guests++
		}
	}
	for _, r := range s.reservations {
		if r.Owner == nodeID {
			reservations++
		}
	}
	return guests, reservations
}

// applyGuest applies a guest entry (D12–D15). It changes s only when it
// returns nil; unchanged reports a success that changed nothing.
func (s *state) applyGuest(e Entry, index uint64) (ch change, err error) {
	guests, res := maps.Clone(s.guests), maps.Clone(s.reservations)
	if guests == nil {
		guests = map[string]Guest{}
	}
	if res == nil {
		res = map[string]Reservation{}
	}
	switch e.Type {
	case TypeGuestReserve:
		var d GuestReserve
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if err := s.checkOwner(d.Owner); err != nil {
			return change{}, err
		}
		if !ValidGuestKind(d.Kind) || !ValidGuestName(d.Name) || !ValidGuestKey(d.Key) || d.At <= 0 {
			return change{}, refuse(CodeInvalid, "the reservation needs a kind (vm or container), a valid name, a key and a time")
		}
		k := resKey(d.Kind, d.Name)
		if r, ok := res[k]; ok && r.Owner == d.Owner && r.Key == d.Key {
			return change{unchanged: true}, nil
		}
		for _, g := range s.guests {
			if g.Kind == d.Kind && g.Name == d.Name && g.Owner == d.Owner && g.Key == d.Key {
				return change{unchanged: true}, nil
			}
		}
		if r, ok := res[k]; ok {
			return change{}, refuse(CodeNameTaken, "the %s name %q is reserved by node %s", d.Kind, d.Name, s.holderName(r.Owner))
		}
		if g, ok := s.guestNamed(d.Kind, d.Name); ok {
			return change{}, refuse(CodeNameTaken, "the %s name %q belongs to a guest of node %s", d.Kind, d.Name, s.holderName(g.Owner))
		}
		if s.guestCount() >= MaxGuests {
			return change{}, refuse(CodeTooManyGuests, "the cluster holds %d guests and reservations, the most it may", MaxGuests)
		}
		res[k] = Reservation{Kind: d.Kind, Name: d.Name, Owner: d.Owner, Key: d.Key, At: d.At, Index: index}

	case TypeGuestRelease:
		var d GuestRelease
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if !ValidGuestKind(d.Kind) || !ValidGuestName(d.Name) || !ValidGuestKey(d.Key) || !ValidNodeID(d.Owner) {
			return change{}, refuse(CodeInvalid, "the release needs a kind, a valid name, a key and an owner")
		}
		k := resKey(d.Kind, d.Name)
		r, ok := res[k]
		if !ok || r.Owner != d.Owner || r.Key != d.Key {
			return change{unchanged: true}, nil
		}
		delete(res, k)

	case TypeGuestAdd:
		var d GuestAdd
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if !ValidNodeID(d.ID) || !ValidGuestKind(d.Kind) || !ValidGuestName(d.Name) || d.At <= 0 ||
			(d.TaskID != "" && !ValidNodeID(d.TaskID)) {
			return change{}, refuse(CodeInvalid, "the guest needs a lower-case UUID, a kind (vm or container), a valid name, a time, and a task ID that is empty or a UUID")
		}
		if err := s.checkOwner(d.Owner); err != nil {
			return change{}, err
		}
		if g, ok := guests[d.ID]; ok {
			if g.Owner == d.Owner {
				return change{unchanged: true}, nil
			}
			return change{}, refuse(CodeNotOwner, "the guest %s belongs to node %s", d.ID, s.holderName(g.Owner))
		}
		k := resKey(d.Kind, d.Name)
		key := ""
		if r, ok := res[k]; ok && r.Owner == d.Owner {
			key = r.Key
			delete(res, k)
		}
		// The total after the entry stays within the limit; a consumed
		// reservation makes room for its guest.
		if len(guests)+len(res) >= MaxGuests {
			return change{}, refuse(CodeTooManyGuests, "the cluster holds %d guests and reservations, the most it may", MaxGuests)
		}
		// Never refused for its name (D14): a second guest of a name is
		// flagged when read (D16).
		guests[d.ID] = Guest{ID: d.ID, Kind: d.Kind, Name: d.Name, Owner: d.Owner, Key: key, TaskID: d.TaskID, AddedIndex: index, At: d.At}

	case TypeGuestRemove:
		var d GuestRemove
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if !ValidNodeID(d.ID) || !ValidNodeID(d.Owner) {
			return change{}, refuse(CodeInvalid, "the removal needs a guest ID and an owner")
		}
		g, ok := guests[d.ID]
		if !ok {
			return change{unchanged: true}, nil
		}
		if g.Owner != d.Owner {
			return change{}, refuse(CodeNotOwner, "the guest %s belongs to node %s", d.ID, s.holderName(g.Owner))
		}
		delete(guests, d.ID)
	default:
		return change{}, fmt.Errorf("not a guest entry type: %s", e.Type)
	}
	s.guests, s.reservations, s.guestsIndex = guests, res, index
	return change{}, nil
}

// sortedGuests returns the guests sorted by ID, each with its computed
// flag (D16): among guests of the same kind and name, every one but the
// one added first is flagged.
func (s state) sortedGuests() []Guest {
	first := map[string]Guest{}
	for _, g := range s.guests {
		k := resKey(g.Kind, g.Name)
		if f, ok := first[k]; !ok || g.AddedIndex < f.AddedIndex {
			first[k] = g
		}
	}
	out := make([]Guest, 0, len(s.guests))
	for _, id := range slices.Sorted(maps.Keys(s.guests)) {
		g := s.guests[id]
		g.Flagged = first[resKey(g.Kind, g.Name)].ID != g.ID
		out = append(out, g)
	}
	return out
}

// sortedReservations returns the reservations sorted by kind and name.
func (s state) sortedReservations() []Reservation {
	out := make([]Reservation, 0, len(s.reservations))
	for _, k := range slices.Sorted(maps.Keys(s.reservations)) {
		out = append(out, s.reservations[k])
	}
	return out
}

// Guests returns the inventory: the guests with their flags, the
// reservations, and the guests index.
func (m *Machine) Guests() Guests {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Guests{ClusterID: m.st.clusterID, Index: m.st.guestsIndex, Guests: m.st.sortedGuests(), Reservations: m.st.sortedReservations()}
}

// describeGuest returns what a guest entry names for the audit (D20), by
// the state before it.
func (s state) describeGuest(e Entry) (a AuditEntry) {
	switch e.Type {
	case TypeGuestReserve:
		var d GuestReserve
		if decodeData(e.Data, &d) == nil {
			a.GuestKind, a.GuestName, a.GuestOwner, a.At = d.Kind, d.Name, d.Owner, d.At
		}
	case TypeGuestRelease:
		var d GuestRelease
		if decodeData(e.Data, &d) == nil {
			a.GuestKind, a.GuestName, a.GuestOwner = d.Kind, d.Name, d.Owner
		}
	case TypeGuestAdd:
		var d GuestAdd
		if decodeData(e.Data, &d) == nil {
			a.GuestID, a.GuestKind, a.GuestName, a.GuestOwner, a.At = d.ID, d.Kind, d.Name, d.Owner, d.At
		}
	case TypeGuestRemove:
		var d GuestRemove
		if decodeData(e.Data, &d) == nil {
			a.GuestID, a.GuestOwner = d.ID, d.Owner
			if g, ok := s.guests[d.ID]; ok {
				a.GuestKind, a.GuestName = g.Kind, g.Name
			}
		}
	}
	return a
}
