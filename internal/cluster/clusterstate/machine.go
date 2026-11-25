package clusterstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
)

// Member states (ADR 0013 B1, amendment 1).
const (
	// StateJoining is a member entry created by join step 1. The node is
	// not a voter yet.
	StateJoining = "joining"
	// StateMember is a full member.
	StateMember = "member"
	// StateRemoving is a member being removed (removal step 0). Its keys
	// are accepted until the Raft configuration without it takes effect.
	StateRemoving = "removing"
)

// Member is one node's entry in the Skep membership (ADR 0013 B1).
type Member struct {
	// ID is the node ID, the UUID its agent created.
	ID string `json:"id"`
	// Name is the node's name.
	Name string `json:"name"`
	// Address is where other nodes reach it ("ip:8444"), or "" for a
	// single host without a listener.
	Address string `json:"address"`
	// Pin is the pin of the node key.
	Pin string `json:"pin"`
	// APIPin is the pin of skep-api's node-to-node key. Empty until
	// step 8e creates that key.
	APIPin string `json:"api_pin,omitempty"`
	// State is the member's state.
	State string `json:"state"`
	// JoinedIndex is the log index of the join step 1 entry that created
	// this member entry; 0 for the founding member. The promotion rule
	// waits until every member has applied it.
	JoinedIndex uint64 `json:"joined_index,omitempty"`
	// JoinOp is the operation ID of that entry.
	JoinOp string `json:"join_op,omitempty"`
}

// Pending is an admitted host that has not joined yet.
type Pending struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Pin     string `json:"pin"`
	// ExpiresAt is in Unix seconds, by the clock of the leader that
	// proposed the entry.
	ExpiresAt int64 `json:"expires_at"`
	// OpID is the operation ID of the pending.create entry.
	OpID string `json:"op_id"`
}

// Stall says why the machine stopped applying entries.
type Stall struct {
	Index  uint64 `json:"index"`
	Reason string `json:"reason"`
}

// View is a copy of the replicated state.
type View struct {
	ClusterID string
	// Epoch is the membership epoch (ADR 0013 B4). It is 1 from
	// cluster.init until "recover" (step 8g) raises it.
	Epoch uint64
	// Members is sorted by ID.
	Members []Member
	// Pendings is sorted by node ID.
	Pendings []Pending
	// AppliedIndex is the index of the last entry the machine applied
	// (or refused, or found a duplicate). It is the machine's own, not the
	// library's, which also counts entries after a stall and the
	// library's own entries.
	AppliedIndex uint64
	// MembershipIndex is the index of the last entry that changed the
	// membership (not counting pending entries).
	MembershipIndex uint64
	// RemovedIndex is the index of the last member.removed entry
	// applied, 0 if none. Health on every member names the members whose
	// state machine has not reached it (decision D12 of step 8b-2a).
	RemovedIndex uint64
	// Stalled is set when the machine met an entry it cannot apply.
	Stalled *Stall
}

// Membership is what the applied-membership file holds (ADR 0013
// amendment 1, point 5): no pending entries.
type Membership struct {
	ClusterID string
	Epoch     uint64
	// Index is the log index this membership reflects.
	Index   uint64
	Members []Member
}

// MembershipWriter makes a new membership durable before the machine
// reports the entry that changed it as applied. An error stops the machine
// at that entry.
type MembershipWriter func(Membership) error

// Event is a membership event of an applied entry, for the log line every
// node writes (ADR 0013 B10; the cluster audit is the next run).
type Event struct {
	// Kind is for example "pending created" or "member joining".
	Kind   string
	NodeID string
	Name   string
}

// Result is the outcome of applying one entry.
type Result struct {
	// Duplicate is true when the entry's operation ID was applied
	// before; nothing changed.
	Duplicate bool
	// Err is why the entry changed nothing: it was refused by the
	// state (ErrRefused) or the machine is stalled (ErrStalled).
	Err error
	// Entry is the decoded entry, when it could be decoded.
	Entry Entry
	// Events are the membership events the entry caused.
	Events []Event
}

// ErrStalled is returned once the machine has met an entry it cannot
// apply. It applies nothing after it (ADR 0013 B5: stop, never skip).
var ErrStalled = errors.New("the replicated state machine has stopped at an entry this build cannot apply")

// ErrRefused wraps the reason a well-formed entry changed nothing.
var ErrRefused = errors.New("entry refused")

// state is the replicated state proper.
type state struct {
	clusterID string
	epoch     uint64
	members   map[string]Member
	pendings  map[string]Pending
	// removedIndex is the index of the last member.removed entry.
	removedIndex uint64
}

func (s state) clone() state {
	return state{clusterID: s.clusterID, epoch: s.epoch, members: maps.Clone(s.members), pendings: maps.Clone(s.pendings),
		removedIndex: s.removedIndex}
}

func (s state) sortedMembers() []Member {
	out := make([]Member, 0, len(s.members))
	for _, id := range slices.Sorted(maps.Keys(s.members)) {
		out = append(out, s.members[id])
	}
	return out
}

func (s state) sortedPendings() []Pending {
	out := make([]Pending, 0, len(s.pendings))
	for _, id := range slices.Sorted(maps.Keys(s.pendings)) {
		out = append(out, s.pendings[id])
	}
	return out
}

// Machine is the replicated state machine. Apply must be called with
// committed entries in log order; reads may come from any goroutine.
type Machine struct {
	mu sync.RWMutex
	st state
	// ops holds the operation IDs applied so far, with the index that
	// applied them.
	ops       map[string]uint64
	applied   uint64
	memIndex  uint64
	stalled   *Stall
	writer    MembershipWriter
	onApplied func()
}

// New returns an empty machine.
func New() *Machine {
	return &Machine{st: state{members: map[string]Member{}, pendings: map[string]Pending{}}, ops: map[string]uint64{}}
}

// SetMembershipWriter installs the writer of the applied-membership file.
// It must be set before the first Apply or Restore.
func (m *Machine) SetMembershipWriter(w MembershipWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writer = w
}

// SetOnApplied installs a function called, without the machine's lock,
// after every entry that changed the membership or the pending entries,
// and after a restore. The listener uses it to close connections of keys
// that are no longer accepted.
func (m *Machine) SetOnApplied(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onApplied = f
}

// Apply applies the entry at the given log index.
func (m *Machine) Apply(index uint64, raw []byte) Result {
	res, notify := m.apply(index, raw)
	if notify != nil {
		notify()
	}
	return res
}

func (m *Machine) apply(index uint64, raw []byte) (Result, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stalled != nil {
		return Result{Err: ErrStalled}, nil
	}
	stall := func(reason string) (Result, func()) {
		m.stalled = &Stall{Index: index, Reason: reason}
		return Result{Err: fmt.Errorf("%w: index %d: %s", ErrStalled, index, reason)}, m.onApplied
	}
	e, err := Decode(raw)
	if err != nil {
		return stall(err.Error())
	}
	max, ok := Supported[e.Type]
	if !ok {
		return stall(fmt.Sprintf("entry type %q is unknown to this build", e.Type))
	}
	if e.Version > max {
		return stall(fmt.Sprintf("entry type %q has version %d; this build applies up to %d", e.Type, e.Version, max))
	}
	if _, done := m.ops[e.OpID]; done {
		m.applied = index
		return Result{Duplicate: true, Entry: e}, nil
	}
	next := m.st.clone()
	ch, err := next.apply(e, index)
	var bad *malformedError
	if errors.As(err, &bad) {
		return stall(bad.Error())
	}
	if err != nil {
		// Refused: nothing changes, the same on every node.
		m.applied = index
		return Result{Err: fmt.Errorf("%w: %s", ErrRefused, err), Entry: e}, nil
	}
	if ch.membership && m.writer != nil {
		rec := Membership{ClusterID: next.clusterID, Epoch: next.epoch, Index: index, Members: next.sortedMembers()}
		if werr := m.writer(rec); werr != nil {
			// The entry is not applied and not reported as applied
			// (ADR 0013 amendment 1, point 5).
			return stall("cannot write the applied-membership file: " + werr.Error())
		}
	}
	m.st = next
	m.ops[e.OpID] = index
	m.applied = index
	if ch.membership {
		m.memIndex = index
	}
	var notify func()
	if ch.membership || ch.pending {
		notify = m.onApplied
	}
	return Result{Entry: e, Events: ch.events}, notify
}

// change says what an applied entry changed.
type change struct {
	membership bool
	pending    bool
	events     []Event
}

// malformedError is data a build that supports the version cannot read:
// the log is not what this build would have written, so it stops.
type malformedError struct{ err error }

func (e *malformedError) Error() string { return e.err.Error() }

func decodeOrStall(e Entry, v any) error {
	if err := decodeData(e.Data, v); err != nil {
		return &malformedError{fmt.Errorf("%s data: %w", e.Type, err)}
	}
	return nil
}

// checkMemberValues checks the values of a member entry.
func checkMemberValues(mem Member, needAddress bool) error {
	switch {
	case !ValidNodeID(mem.ID):
		return fmt.Errorf("node ID %q is not a node ID", mem.ID)
	case !ValidName(mem.Name):
		return fmt.Errorf("node name %q is not valid", mem.Name)
	case !ValidPin(mem.Pin):
		return fmt.Errorf("pin %q is not a key pin", mem.Pin)
	case mem.APIPin != "":
		return errors.New("an API pin is not accepted before step 8e")
	case mem.Address == "" && needAddress:
		return errors.New("an address is required")
	}
	if mem.Address != "" {
		if err := CheckAddress(mem.Address); err != nil {
			return err
		}
	}
	return nil
}

// conflict returns why a new node with these values would duplicate a
// member or a pending entry, ignoring the pending entry of skipPending.
func (s state) conflict(id, name, address, pin, skipPending string) error {
	for _, mem := range s.sortedMembers() {
		switch {
		case mem.ID == id:
			return fmt.Errorf("node ID %s is already a member (%s)", id, mem.Name)
		case mem.Name == name:
			return fmt.Errorf("the name %s belongs to member %s", name, mem.ID)
		case address != "" && mem.Address == address:
			return fmt.Errorf("the address %s belongs to member %s", address, mem.Name)
		case mem.Pin == pin:
			return fmt.Errorf("the pin %s belongs to member %s", pin, mem.Name)
		}
	}
	for _, p := range s.sortedPendings() {
		if p.NodeID == skipPending {
			continue
		}
		switch {
		case p.NodeID == id:
			return fmt.Errorf("node ID %s is already pending (%s)", id, p.Name)
		case p.Name == name:
			return fmt.Errorf("the name %s belongs to the pending entry of %s", name, p.NodeID)
		case p.Address == address:
			return fmt.Errorf("the address %s belongs to the pending entry of %s", address, p.Name)
		case p.Pin == pin:
			return fmt.Errorf("the pin %s belongs to the pending entry of %s", pin, p.Name)
		}
	}
	return nil
}

func (s *state) apply(e Entry, index uint64) (change, error) {
	if e.Type != TypeClusterInit && s.clusterID == "" {
		return change{}, errors.New("the cluster does not exist yet")
	}
	switch e.Type {
	case TypeClusterInit:
		var d ClusterInit
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if s.clusterID != "" {
			return change{}, fmt.Errorf("the cluster already exists (%s)", s.clusterID)
		}
		if !ValidNodeID(d.ClusterID) {
			return change{}, fmt.Errorf("cluster ID %q is not a UUID", d.ClusterID)
		}
		if err := checkMemberValues(d.Member, false); err != nil {
			return change{}, err
		}
		if d.Member.State != StateMember || d.Member.JoinedIndex != 0 || d.Member.JoinOp != "" {
			return change{}, errors.New("the founding member must be in state member")
		}
		s.clusterID, s.epoch = d.ClusterID, 1
		s.members[d.Member.ID] = d.Member
		return change{membership: true, events: []Event{{Kind: "member", NodeID: d.Member.ID, Name: d.Member.Name}}}, nil

	case TypeMemberAddress:
		var d MemberAddress
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		mem, ok := s.members[d.NodeID]
		if !ok {
			return change{}, fmt.Errorf("node %s is not a member", d.NodeID)
		}
		if len(s.members) != 1 || len(s.pendings) != 0 {
			return change{}, errors.New("a member's address is changed only while it is the only member and nothing is pending")
		}
		if d.Address != "" {
			if err := CheckAddress(d.Address); err != nil {
				return change{}, err
			}
		}
		mem.Address = d.Address
		s.members[d.NodeID] = mem
		return change{membership: true}, nil

	case TypePendingCreate:
		var d PendingCreate
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if err := checkMemberValues(Member{ID: d.NodeID, Name: d.Name, Address: d.Address, Pin: d.Pin}, true); err != nil {
			return change{}, err
		}
		if d.ExpiresAt <= 0 {
			return change{}, errors.New("a pending entry needs an expiry time")
		}
		for _, mem := range s.sortedMembers() {
			if mem.Address == "" {
				return change{}, fmt.Errorf("member %s has no address: nobody could join it", mem.Name)
			}
		}
		if err := s.conflict(d.NodeID, d.Name, d.Address, d.Pin, ""); err != nil {
			return change{}, err
		}
		s.pendings[d.NodeID] = Pending{NodeID: d.NodeID, Name: d.Name, Address: d.Address, Pin: d.Pin, ExpiresAt: d.ExpiresAt, OpID: e.OpID}
		return change{pending: true, events: []Event{{Kind: "pending created", NodeID: d.NodeID, Name: d.Name}}}, nil

	case TypePendingWithdraw:
		var d PendingWithdraw
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		p, ok := s.pendings[d.NodeID]
		if !ok || p.Name != d.Name {
			return change{}, fmt.Errorf("there is no pending entry for %s (%s)", d.Name, d.NodeID)
		}
		delete(s.pendings, d.NodeID)
		return change{pending: true, events: []Event{{Kind: "pending withdrawn", NodeID: p.NodeID, Name: p.Name}}}, nil

	case TypePendingExpire:
		var d PendingExpire
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		p, ok := s.pendings[d.NodeID]
		if !ok || p.ExpiresAt != d.ExpiresAt {
			return change{}, fmt.Errorf("there is no pending entry for %s expiring at %d", d.NodeID, d.ExpiresAt)
		}
		if d.At < p.ExpiresAt {
			return change{}, fmt.Errorf("the pending entry of %s expires at %d, after %d", p.Name, p.ExpiresAt, d.At)
		}
		delete(s.pendings, d.NodeID)
		return change{pending: true, events: []Event{{Kind: "pending expired", NodeID: p.NodeID, Name: p.Name}}}, nil

	case TypeMemberJoin:
		var d MemberJoin
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		p, ok := s.pendings[d.NodeID]
		if !ok {
			return change{}, fmt.Errorf("there is no pending entry for %s", d.NodeID)
		}
		if d.Pin != p.Pin {
			return change{}, fmt.Errorf("the pin %s is not the pending entry's", d.Pin)
		}
		if d.At >= p.ExpiresAt {
			return change{}, fmt.Errorf("the pending entry of %s expired at %d (join at %d)", p.Name, p.ExpiresAt, d.At)
		}
		if err := s.conflict(p.NodeID, p.Name, p.Address, p.Pin, p.NodeID); err != nil {
			return change{}, err
		}
		delete(s.pendings, d.NodeID)
		s.members[p.NodeID] = Member{ID: p.NodeID, Name: p.Name, Address: p.Address, Pin: p.Pin,
			State: StateJoining, JoinedIndex: index, JoinOp: e.OpID}
		return change{membership: true, pending: true, events: []Event{
			{Kind: "pending used", NodeID: p.NodeID, Name: p.Name},
			{Kind: "member joining", NodeID: p.NodeID, Name: p.Name},
		}}, nil

	case TypeMemberPromoted:
		var d MemberPromoted
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		mem, ok := s.members[d.NodeID]
		if !ok {
			return change{}, fmt.Errorf("node %s is not a member", d.NodeID)
		}
		if mem.State != StateJoining {
			return change{}, fmt.Errorf("member %s is in state %s, not joining", mem.Name, mem.State)
		}
		mem.State = StateMember
		s.members[d.NodeID] = mem
		return change{membership: true, events: []Event{{Kind: "member", NodeID: mem.ID, Name: mem.Name}}}, nil

	case TypeMemberRemoving:
		var d MemberRemoving
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		mem, ok := s.members[d.NodeID]
		if !ok {
			return change{}, fmt.Errorf("node %s is not a member", d.NodeID)
		}
		if len(d.ForgetGuests) != 0 {
			return change{}, errors.New("guest entries cannot be named in this version of the entry (the guest inventory comes with step 8d)")
		}
		if mem.State == StateRemoving {
			return change{}, fmt.Errorf("member %s is already being removed", mem.Name)
		}
		// Decision D7a: no removal of another member while one joins.
		for _, other := range s.sortedMembers() {
			if other.ID != mem.ID && other.State == StateJoining {
				return change{}, fmt.Errorf("member %s is joining; %s cannot be removed until that join has finished or %s is removed", other.Name, mem.Name, other.Name)
			}
		}
		// Decision D5g: never the last member that is not being removed.
		rest := 0
		for _, other := range s.sortedMembers() {
			if other.ID != mem.ID && other.State != StateRemoving {
				rest++
			}
		}
		if rest == 0 {
			return change{}, fmt.Errorf("%s is the only member that is not being removed", mem.Name)
		}
		mem.State = StateRemoving
		s.members[d.NodeID] = mem
		ch := change{membership: true, events: []Event{{Kind: "member removing", NodeID: mem.ID, Name: mem.Name}}}
		// Decision D7b: every pending admission is withdrawn in the same
		// apply, so an admission issued before a removal is void.
		for _, p := range s.sortedPendings() {
			delete(s.pendings, p.NodeID)
			ch.pending = true
			ch.events = append(ch.events, Event{Kind: "pending withdrawn", NodeID: p.NodeID, Name: p.Name})
		}
		return ch, nil

	case TypeMemberRemoved:
		var d MemberRemoved
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		mem, ok := s.members[d.NodeID]
		if !ok {
			return change{}, fmt.Errorf("node %s is not a member", d.NodeID)
		}
		if mem.State != StateRemoving {
			return change{}, fmt.Errorf("member %s is in state %s, not removing", mem.Name, mem.State)
		}
		delete(s.members, d.NodeID)
		s.removedIndex = index
		return change{membership: true, events: []Event{{Kind: "member removed", NodeID: mem.ID, Name: mem.Name}}}, nil
	}
	// A type a build declares in Supported without a case here changes
	// nothing (as in step 8a); TestEveryTypeHasAnApplyStep keeps every
	// type of this build in the switch.
	return change{}, nil
}

// View returns a copy of the state.
func (m *Machine) View() View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := View{ClusterID: m.st.clusterID, Epoch: m.st.epoch, AppliedIndex: m.applied, MembershipIndex: m.memIndex,
		RemovedIndex: m.st.removedIndex, Members: m.st.sortedMembers(), Pendings: m.st.sortedPendings()}
	if m.stalled != nil {
		s := *m.stalled
		v.Stalled = &s
	}
	return v
}

// Member returns one member's entry.
func (m *Machine) Member(id string) (Member, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.st.members[id]
	return mem, ok
}

// AppliedIndex returns the machine's own applied index and whether it is
// stalled.
func (m *Machine) AppliedIndex() (uint64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.applied, m.stalled != nil
}

// snapshotFormat is the version of the snapshot encoding. Format 1 (step
// 8a) has no pending entries and no epoch; format 2 (8b-1) no removal
// index. Both are still read.
const snapshotFormat = 3

type snapshot struct {
	Format          int               `json:"format"`
	ClusterID       string            `json:"cluster_id"`
	Epoch           uint64            `json:"epoch,omitempty"`
	Members         []Member          `json:"members"`
	Pendings        []Pending         `json:"pendings,omitempty"`
	Ops             map[string]uint64 `json:"ops"`
	Applied         uint64            `json:"applied"`
	MembershipIndex uint64            `json:"membership_index,omitempty"`
	RemovedIndex    uint64            `json:"removed_index,omitempty"`
	// Stalled is read from snapshots written by the first 8a build and
	// ignored; it is never written. A stall belongs to the running build,
	// not to the state: a later build that can apply the entry must.
	Stalled *Stall `json:"stalled,omitempty"`
}

// ErrSnapshotWhileStalled is returned by Snapshot on a stalled machine. The
// library then takes no snapshot, so it never truncates the log past the
// entry the machine stopped at, and a build that can apply it still finds
// it and everything after it in the log.
var ErrSnapshotWhileStalled = errors.New("clusterstate: no snapshot while the state machine is stopped at an entry it cannot apply")

// Snapshot returns the state encoded for a snapshot. It refuses while the
// machine is stalled.
func (m *Machine) Snapshot() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stalled != nil {
		return nil, fmt.Errorf("%w (index %d)", ErrSnapshotWhileStalled, m.stalled.Index)
	}
	s := snapshot{Format: snapshotFormat, ClusterID: m.st.clusterID, Epoch: m.st.epoch, Ops: maps.Clone(m.ops),
		Applied: m.applied, MembershipIndex: m.memIndex, RemovedIndex: m.st.removedIndex, Members: m.st.sortedMembers(), Pendings: m.st.sortedPendings()}
	return json.Marshal(s)
}

// Restore replaces the state with a snapshot's. A snapshot changes the
// membership too: the applied-membership file is written first, by the
// same rules as for an entry, and a failed write leaves the state as it
// was and returns the error.
func (m *Machine) Restore(r io.Reader) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var s snapshot
	if err := dec.Decode(&s); err != nil {
		return fmt.Errorf("clusterstate: restore: %w", err)
	}
	if s.Format < 1 || s.Format > snapshotFormat {
		return fmt.Errorf("clusterstate: snapshot format %d is unknown to this build", s.Format)
	}
	st := state{clusterID: s.ClusterID, epoch: s.Epoch, members: map[string]Member{}, pendings: map[string]Pending{}, removedIndex: s.RemovedIndex}
	if s.Format == 1 && st.clusterID != "" {
		st.epoch = 1
	}
	for _, mem := range s.Members {
		st.members[mem.ID] = mem
	}
	for _, p := range s.Pendings {
		st.pendings[p.NodeID] = p
	}
	if s.Ops == nil {
		s.Ops = map[string]uint64{}
	}
	memIndex := s.MembershipIndex
	if memIndex == 0 && st.clusterID != "" {
		// A format 1 snapshot does not say; its applied index is an
		// upper bound, and the file is never written back to a lower
		// one anyway.
		memIndex = s.Applied
	}
	m.mu.Lock()
	if m.writer != nil && st.clusterID != "" {
		rec := Membership{ClusterID: st.clusterID, Epoch: st.epoch, Index: memIndex, Members: st.sortedMembers()}
		if err := m.writer(rec); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("clusterstate: restore: cannot write the applied-membership file: %w", err)
		}
	}
	// s.Stalled is ignored: this build applies again whatever stalled an
	// earlier one, and stalls again by itself if it cannot.
	m.st, m.ops, m.applied, m.memIndex, m.stalled = st, s.Ops, s.Applied, memIndex, nil
	notify := m.onApplied
	m.mu.Unlock()
	if notify != nil {
		notify()
	}
	return nil
}
