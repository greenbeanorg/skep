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

// Member states. 8a knows one; "joining" arrives with step 8b.
const (
	StateMember = "member"
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
}

// Stall says why the machine stopped applying entries.
type Stall struct {
	Index  uint64 `json:"index"`
	Reason string `json:"reason"`
}

// View is a copy of the replicated state.
type View struct {
	ClusterID string
	// Members is sorted by ID.
	Members []Member
	// AppliedIndex is the index of the last entry the machine saw.
	AppliedIndex uint64
	// Stalled is set when the machine met an entry it cannot apply.
	Stalled *Stall
}

// Result is the outcome of applying one entry.
type Result struct {
	// Duplicate is true when the entry's operation ID was applied
	// before; nothing changed.
	Duplicate bool
	// Err is why the entry changed nothing: it was refused by the
	// state (ErrRefused) or the machine is stalled (ErrStalled).
	Err error
}

// ErrStalled is returned once the machine has met an entry it cannot
// apply. It applies nothing after it (ADR 0013 B5: stop, never skip).
var ErrStalled = errors.New("the replicated state machine has stopped at an entry this build cannot apply")

// ErrRefused wraps the reason a well-formed entry changed nothing.
var ErrRefused = errors.New("entry refused")

// Machine is the replicated state machine. Apply must be called with
// committed entries in log order; reads may come from any goroutine.
type Machine struct {
	mu        sync.RWMutex
	clusterID string
	members   map[string]Member
	// ops holds the operation IDs applied so far, with the index that
	// applied them.
	ops     map[string]uint64
	applied uint64
	stalled *Stall
}

// New returns an empty machine.
func New() *Machine {
	return &Machine{members: map[string]Member{}, ops: map[string]uint64{}}
}

// Apply applies the entry at the given log index.
func (m *Machine) Apply(index uint64, raw []byte) Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stalled != nil {
		return Result{Err: ErrStalled}
	}
	stall := func(reason string) Result {
		m.stalled = &Stall{Index: index, Reason: reason}
		return Result{Err: fmt.Errorf("%w: index %d: %s", ErrStalled, index, reason)}
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
		return Result{Duplicate: true}
	}
	switch e.Type {
	case TypeClusterInit:
		err = m.applyClusterInit(e)
	case TypeMemberAddress:
		err = m.applyMemberAddress(e)
	}
	var bad *malformedError
	if errors.As(err, &bad) {
		return stall(bad.Error())
	}
	// From here the entry is dealt with, whether it changed the state
	// or was refused by it.
	m.applied = index
	if err != nil {
		return Result{Err: fmt.Errorf("%w: %s", ErrRefused, err)}
	}
	m.ops[e.OpID] = index
	return Result{}
}

// malformedError is data a build that supports the version cannot read:
// the log is not what this build would have written, so it stops.
type malformedError struct{ err error }

func (e *malformedError) Error() string { return e.err.Error() }

func validMember(mem Member) error {
	if mem.ID == "" || mem.Name == "" || mem.Pin == "" {
		return errors.New("a member needs an ID, a name and a pin")
	}
	if mem.State != StateMember {
		return fmt.Errorf("member state %q is unknown to this build", mem.State)
	}
	return nil
}

func (m *Machine) applyClusterInit(e Entry) error {
	var d ClusterInit
	if err := decodeData(e.Data, &d); err != nil {
		return &malformedError{fmt.Errorf("%s data: %w", e.Type, err)}
	}
	if d.ClusterID == "" {
		return &malformedError{errors.New("cluster.init without a cluster ID")}
	}
	if err := validMember(d.Member); err != nil {
		return &malformedError{err}
	}
	if m.clusterID != "" {
		return fmt.Errorf("the cluster already exists (%s)", m.clusterID)
	}
	m.clusterID = d.ClusterID
	m.members[d.Member.ID] = d.Member
	return nil
}

func (m *Machine) applyMemberAddress(e Entry) error {
	var d MemberAddress
	if err := decodeData(e.Data, &d); err != nil {
		return &malformedError{fmt.Errorf("%s data: %w", e.Type, err)}
	}
	mem, ok := m.members[d.NodeID]
	if !ok {
		return fmt.Errorf("node %s is not a member", d.NodeID)
	}
	for id, other := range m.members {
		if id != d.NodeID && d.Address != "" && other.Address == d.Address {
			return fmt.Errorf("address %s belongs to member %s", d.Address, other.Name)
		}
	}
	mem.Address = d.Address
	m.members[d.NodeID] = mem
	return nil
}

// View returns a copy of the state.
func (m *Machine) View() View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := View{ClusterID: m.clusterID, AppliedIndex: m.applied}
	for _, id := range slices.Sorted(maps.Keys(m.members)) {
		v.Members = append(v.Members, m.members[id])
	}
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
	mem, ok := m.members[id]
	return mem, ok
}

// PinnedNode returns the ID of the member whose node key has this pin.
// The node-to-node listener accepts a key only when this finds one.
func (m *Machine) PinnedNode(pin string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stalled != nil || pin == "" {
		// A stalled machine no longer knows the membership; accept
		// nobody rather than a possibly removed key.
		return "", false
	}
	for id, mem := range m.members {
		if mem.Pin == pin {
			return id, true
		}
	}
	return "", false
}

// snapshotFormat is the version of the snapshot encoding.
const snapshotFormat = 1

type snapshot struct {
	Format    int               `json:"format"`
	ClusterID string            `json:"cluster_id"`
	Members   []Member          `json:"members"`
	Ops       map[string]uint64 `json:"ops"`
	Applied   uint64            `json:"applied"`
	Stalled   *Stall            `json:"stalled,omitempty"`
}

// Snapshot returns the state encoded for a snapshot.
func (m *Machine) Snapshot() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := snapshot{Format: snapshotFormat, ClusterID: m.clusterID, Ops: maps.Clone(m.ops), Applied: m.applied, Stalled: m.stalled}
	for _, id := range slices.Sorted(maps.Keys(m.members)) {
		s.Members = append(s.Members, m.members[id])
	}
	return json.Marshal(s)
}

// Restore replaces the state with a snapshot's.
func (m *Machine) Restore(r io.Reader) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var s snapshot
	if err := dec.Decode(&s); err != nil {
		return fmt.Errorf("clusterstate: restore: %w", err)
	}
	if s.Format != snapshotFormat {
		return fmt.Errorf("clusterstate: snapshot format %d is unknown to this build", s.Format)
	}
	members := map[string]Member{}
	for _, mem := range s.Members {
		members[mem.ID] = mem
	}
	if s.Ops == nil {
		s.Ops = map[string]uint64{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusterID, m.members, m.ops, m.applied, m.stalled = s.ClusterID, members, s.Ops, s.Applied, s.Stalled
	return nil
}
