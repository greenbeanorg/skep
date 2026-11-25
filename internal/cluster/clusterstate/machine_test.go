package clusterstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	clusterA = "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"
	nodeA    = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"
	nodeB    = "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22"
	nodeC    = "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33"
)

func pin(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

var (
	pinA = pin('a')
	pinB = pin('b')
	pinC = pin('c')
)

func encode(t *testing.T, typ, op string, data any) []byte {
	t.Helper()
	e, err := NewEntry(typ, op, ActorSystem, nodeA, data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func initEntry(t *testing.T, op string) []byte {
	return encode(t, TypeClusterInit, op, ClusterInit{
		ClusterID: clusterA, Member: Member{ID: nodeA, Name: "smith", Address: "10.0.0.1:8444", Pin: pinA, State: StateMember}})
}

func addressEntry(t *testing.T, op, addr string) []byte {
	return encode(t, TypeMemberAddress, op, MemberAddress{NodeID: nodeA, Address: addr})
}

func pendingEntry(t *testing.T, op, id, name, addr, p string, expires int64) []byte {
	return encode(t, TypePendingCreate, op, PendingCreate{NodeID: id, Name: name, Address: addr, Pin: p, ExpiresAt: expires})
}

func joinEntry(t *testing.T, op, id, p string, at int64) []byte {
	return encode(t, TypeMemberJoin, op, MemberJoin{NodeID: id, Pin: p, At: at})
}

func pinned(m *Machine, p string) (string, bool) {
	v := m.View()
	if v.Stalled != nil {
		return "", false
	}
	for _, mem := range v.Members {
		if mem.Pin == p {
			return mem.ID, true
		}
	}
	return "", false
}

func TestEveryEntryCarriesTypeAndVersion(t *testing.T) {
	for typ := range Supported {
		e, err := NewEntry(typ, "op-1", ActorSystem, nodeA, struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		b, err := e.Encode()
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if raw["type"] != typ || raw["version"] != float64(1) {
			t.Fatalf("%s is written as %s", typ, b)
		}
	}
	// An entry without a type, a version or an operation ID cannot be
	// written or read.
	for _, e := range []Entry{
		{Version: 1, OpID: "a", Actor: "x", Origin: "n"},
		{Type: TypeClusterInit, OpID: "a", Actor: "x", Origin: "n"},
		{Type: TypeClusterInit, Version: 1, Actor: "x", Origin: "n"},
		{Type: TypeClusterInit, Version: 1, OpID: "a"},
	} {
		if _, err := e.Encode(); err == nil {
			t.Errorf("encoded %+v", e)
		}
		b, _ := json.Marshal(e)
		if _, err := Decode(b); err == nil {
			t.Errorf("decoded %s", b)
		}
	}
	if _, err := NewEntry("user.create", "a", "x", "n", nil); err == nil {
		t.Error("NewEntry made an entry of an unknown type")
	}
}

func TestApplyInitAddressAndDuplicate(t *testing.T) {
	m := New()
	if r := m.Apply(1, initEntry(t, "op-init")); r.Err != nil || r.Duplicate {
		t.Fatalf("init: %+v", r)
	}
	if id, ok := pinned(m, pinA); !ok || id != nodeA {
		t.Fatal("the first member's key is not pinned")
	}
	// A second cluster.init is refused and changes nothing.
	b := encode(t, TypeClusterInit, "op-init-2", ClusterInit{
		ClusterID: clusterA, Member: Member{ID: nodeB, Name: "x", Pin: pinB, State: StateMember}})
	if r := m.Apply(2, b); !errors.Is(r.Err, ErrRefused) {
		t.Fatalf("second init: %+v", r)
	}
	if v := m.View(); v.ClusterID != clusterA || len(v.Members) != 1 || v.Stalled != nil || v.Epoch != 1 {
		t.Fatalf("state after a refused entry: %+v", v)
	}
	if r := m.Apply(3, addressEntry(t, "op-addr", "10.0.0.2:8444")); r.Err != nil {
		t.Fatal(r.Err)
	}
	// The same operation ID again is a no-op, even with other content.
	if r := m.Apply(4, addressEntry(t, "op-addr", "10.0.0.9:8444")); !r.Duplicate || r.Err != nil {
		t.Fatalf("duplicate: %+v", r)
	}
	if mem, _ := m.Member(nodeA); mem.Address != "10.0.0.2:8444" {
		t.Fatalf("address %q", mem.Address)
	}
	if v := m.View(); v.AppliedIndex != 4 || v.MembershipIndex != 3 {
		t.Fatalf("applied index %d, membership index %d", v.AppliedIndex, v.MembershipIndex)
	}
}

func TestUnknownEntryStopsTheMachine(t *testing.T) {
	cases := map[string][]byte{
		"unknown type":   []byte(`{"type":"user.create","version":1,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"newer version":  []byte(`{"type":"member.address","version":2,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"newer join":     []byte(`{"type":"member.join","version":2,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"no type":        []byte(`{"version":1,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"not JSON":       []byte(`garbage`),
		"two values":     []byte(`{"type":"member.address","version":1,"op_id":"x","actor":"a","origin":"n","data":{}} {}`),
		"unknown field":  []byte(`{"type":"member.address","version":1,"op_id":"x","actor":"a","origin":"n","data":{},"extra":1}`),
		"malformed data": []byte(`{"type":"member.address","version":1,"op_id":"x","actor":"a","origin":"n","data":{"nope":1}}`),
		"pending data":   []byte(`{"type":"pending.create","version":1,"op_id":"x","actor":"a","origin":"n","data":{"node_id":5}}`),
		"join data":      []byte(`{"type":"member.join","version":1,"op_id":"x","actor":"a","origin":"n","data":[]}`),
	}
	for name, raw := range cases {
		m := New()
		m.Apply(1, initEntry(t, "op-init"))
		if r := m.Apply(2, raw); !errors.Is(r.Err, ErrStalled) {
			t.Fatalf("%s: %+v", name, r)
		}
		v := m.View()
		if v.Stalled == nil || v.Stalled.Index != 2 || v.AppliedIndex != 1 {
			t.Fatalf("%s: view %+v", name, v)
		}
		// Nothing after it is applied: it stops, it does not skip.
		if r := m.Apply(3, addressEntry(t, "op-addr", "10.0.0.2:8444")); !errors.Is(r.Err, ErrStalled) {
			t.Fatalf("%s: applied after the stall: %+v", name, r)
		}
		if mem, _ := m.Member(nodeA); mem.Address != "10.0.0.1:8444" {
			t.Fatalf("%s: an entry after the stall changed the state", name)
		}
		if _, stalled := m.AppliedIndex(); !stalled {
			t.Fatalf("%s: not reported as stalled", name)
		}
	}
}

// machineWithPending is a cluster of A with B pending, expiring at 1000.
func machineWithPending(t *testing.T) *Machine {
	t.Helper()
	m := New()
	if r := m.Apply(1, initEntry(t, "op-init")); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := m.Apply(2, pendingEntry(t, "op-p", nodeB, "langrishe", "10.0.0.2:8444", pinB, 1000)); r.Err != nil {
		t.Fatal(r.Err)
	}
	return m
}

func TestJoinStepsAndEvents(t *testing.T) {
	m := machineWithPending(t)
	if v := m.View(); len(v.Pendings) != 1 || v.MembershipIndex != 1 {
		t.Fatalf("pending: %+v", v)
	}
	r := m.Apply(3, joinEntry(t, "op-j", nodeB, pinB, 999))
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if len(r.Events) != 2 || r.Events[0].Kind != "pending used" || r.Events[1].Kind != "member joining" {
		t.Fatalf("events %+v", r.Events)
	}
	mem, ok := m.Member(nodeB)
	if !ok || mem.State != StateJoining || mem.JoinedIndex != 3 || mem.JoinOp != "op-j" || mem.Address != "10.0.0.2:8444" {
		t.Fatalf("member %+v", mem)
	}
	if v := m.View(); len(v.Pendings) != 0 || v.MembershipIndex != 3 {
		t.Fatalf("after join: %+v", v)
	}
	// The same join again (a repeated call) is a no-op.
	if r := m.Apply(4, joinEntry(t, "op-j", nodeB, pinB, 999)); !r.Duplicate {
		t.Fatalf("repeat %+v", r)
	}
	r = m.Apply(5, encode(t, TypeMemberPromoted, "op-pr", MemberPromoted{NodeID: nodeB}))
	if r.Err != nil || len(r.Events) != 1 || r.Events[0].Kind != "member" {
		t.Fatalf("promoted %+v", r)
	}
	if mem, _ := m.Member(nodeB); mem.State != StateMember {
		t.Fatalf("state %s", mem.State)
	}
}

// Every case of D7: a decodable entry with invalid values is refused and
// changes nothing, the same on every node.
func TestInvalidValuesAreRefused(t *testing.T) {
	type tc struct {
		name  string
		entry func(t *testing.T) []byte
	}
	cases := []tc{
		{"pending: bad node ID", func(t *testing.T) []byte {
			return pendingEntry(t, "x", "NOT-A-UUID", "star", "10.0.0.3:8444", pinC, 1000)
		}},
		{"pending: bad name", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "-star", "10.0.0.3:8444", pinC, 1000) }},
		{"pending: bad pin", func(t *testing.T) []byte {
			return pendingEntry(t, "x", nodeC, "star", "10.0.0.3:8444", "sha256:AB", 1000)
		}},
		{"pending: no address", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "", pinC, 1000) }},
		{"pending: address without port", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.3", pinC, 1000) }},
		{"pending: unspecified address", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "0.0.0.0:8444", pinC, 1000) }},
		{"pending: multicast address", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "224.0.0.1:8444", pinC, 1000) }},
		{"pending: port 0", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.3:0", pinC, 1000) }},
		{"pending: no expiry", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.3:8444", pinC, 0) }},
		{"pending: duplicate member ID", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeA, "star", "10.0.0.3:8444", pinC, 1000) }},
		{"pending: duplicate member name", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "smith", "10.0.0.3:8444", pinC, 1000) }},
		{"pending: duplicate member address", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.1:8444", pinC, 1000) }},
		{"pending: duplicate member pin", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.3:8444", pinA, 1000) }},
		{"pending: duplicate pending ID", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeB, "star", "10.0.0.3:8444", pinC, 1000) }},
		{"pending: duplicate pending name", func(t *testing.T) []byte {
			return pendingEntry(t, "x", nodeC, "langrishe", "10.0.0.3:8444", pinC, 1000)
		}},
		{"pending: duplicate pending address", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.2:8444", pinC, 1000) }},
		{"pending: duplicate pending pin", func(t *testing.T) []byte { return pendingEntry(t, "x", nodeC, "star", "10.0.0.3:8444", pinB, 1000) }},
		{"withdraw: no such pending", func(t *testing.T) []byte {
			return encode(t, TypePendingWithdraw, "x", PendingWithdraw{NodeID: nodeC, Name: "star"})
		}},
		{"withdraw: name does not match", func(t *testing.T) []byte {
			return encode(t, TypePendingWithdraw, "x", PendingWithdraw{NodeID: nodeB, Name: "star"})
		}},
		{"expire: not yet due", func(t *testing.T) []byte {
			return encode(t, TypePendingExpire, "x", PendingExpire{NodeID: nodeB, ExpiresAt: 1000, At: 999})
		}},
		{"expire: another pending entry", func(t *testing.T) []byte {
			return encode(t, TypePendingExpire, "x", PendingExpire{NodeID: nodeB, ExpiresAt: 900, At: 2000})
		}},
		{"expire: no such pending", func(t *testing.T) []byte {
			return encode(t, TypePendingExpire, "x", PendingExpire{NodeID: nodeC, ExpiresAt: 1000, At: 2000})
		}},
		{"join: missing pending", func(t *testing.T) []byte { return joinEntry(t, "x", nodeC, pinC, 10) }},
		{"join: expired pending", func(t *testing.T) []byte { return joinEntry(t, "x", nodeB, pinB, 1000) }},
		{"join: another pin", func(t *testing.T) []byte { return joinEntry(t, "x", nodeB, pinC, 10) }},
		{"promoted: not a member", func(t *testing.T) []byte {
			return encode(t, TypeMemberPromoted, "x", MemberPromoted{NodeID: nodeC})
		}},
		{"promoted: already a member", func(t *testing.T) []byte {
			return encode(t, TypeMemberPromoted, "x", MemberPromoted{NodeID: nodeA})
		}},
		{"address: while a host is pending", func(t *testing.T) []byte { return addressEntry(t, "x", "10.0.0.9:8444") }},
		{"address: not a member", func(t *testing.T) []byte {
			return encode(t, TypeMemberAddress, "x", MemberAddress{NodeID: nodeC, Address: "10.0.0.9:8444"})
		}},
		{"init: second cluster", func(t *testing.T) []byte { return initEntry(t, "x") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Two machines stand for two nodes: both refuse, both
			// stay equal.
			var snaps [][]byte
			for i := 0; i < 2; i++ {
				m := machineWithPending(t)
				before, _ := m.Snapshot()
				r := m.Apply(3, c.entry(t))
				if !errors.Is(r.Err, ErrRefused) {
					t.Fatalf("not refused: %+v", r)
				}
				after, _ := m.Snapshot()
				// Only the applied index moved.
				before = bytes.Replace(before, []byte(`"applied":2`), []byte(`"applied":3`), 1)
				if !bytes.Equal(before, after) {
					t.Fatalf("the state changed:\n%s\n%s", before, after)
				}
				snaps = append(snaps, after)
			}
			if !bytes.Equal(snaps[0], snaps[1]) {
				t.Fatal("two nodes differ after the same entry")
			}
		})
	}
	// Values of the founding member are checked too.
	for name, mem := range map[string]Member{
		"bad pin":   {ID: nodeA, Name: "smith", Pin: "sha256:aa", State: StateMember},
		"joining":   {ID: nodeA, Name: "smith", Pin: pinA, State: StateJoining},
		"API pin":   {ID: nodeA, Name: "smith", Pin: pinA, APIPin: pinB, State: StateMember},
		"bad addr":  {ID: nodeA, Name: "smith", Address: "10.0.0.1:08444", Pin: pinA, State: StateMember},
		"no member": {},
	} {
		m := New()
		r := m.Apply(1, encode(t, TypeClusterInit, "x", ClusterInit{ClusterID: clusterA, Member: mem}))
		if !errors.Is(r.Err, ErrRefused) || m.View().ClusterID != "" {
			t.Errorf("init with %s: %+v", name, r)
		}
	}
	m := New()
	if r := m.Apply(1, encode(t, TypeClusterInit, "x", ClusterInit{ClusterID: "c1",
		Member: Member{ID: nodeA, Name: "smith", Pin: pinA, State: StateMember}})); !errors.Is(r.Err, ErrRefused) {
		t.Errorf("init with a cluster ID that is not a UUID: %+v", r)
	}
	// Nothing but cluster.init before the cluster exists.
	if r := New().Apply(1, pendingEntry(t, "x", nodeB, "b", "10.0.0.2:8444", pinB, 5)); !errors.Is(r.Err, ErrRefused) {
		t.Errorf("pending before the cluster: %+v", r)
	}
	// A pending entry needs every member to have an address.
	m = New()
	m.Apply(1, encode(t, TypeClusterInit, "i", ClusterInit{ClusterID: clusterA, Member: Member{ID: nodeA, Name: "smith", Pin: pinA, State: StateMember}}))
	if r := m.Apply(2, pendingEntry(t, "x", nodeB, "b", "10.0.0.2:8444", pinB, 5)); !errors.Is(r.Err, ErrRefused) {
		t.Errorf("pending with a member without an address: %+v", r)
	}
}

func TestExpiryAndWithdrawal(t *testing.T) {
	m := machineWithPending(t)
	r := m.Apply(3, encode(t, TypePendingExpire, "e", PendingExpire{NodeID: nodeB, ExpiresAt: 1000, At: 1000}))
	if r.Err != nil || len(r.Events) != 1 || r.Events[0].Kind != "pending expired" {
		t.Fatalf("expire %+v", r)
	}
	// Nothing is accepted after the expiry entry: the join is refused.
	if r := m.Apply(4, joinEntry(t, "j", nodeB, pinB, 10)); !errors.Is(r.Err, ErrRefused) {
		t.Fatalf("join after expiry %+v", r)
	}
	m = machineWithPending(t)
	r = m.Apply(3, encode(t, TypePendingWithdraw, "w", PendingWithdraw{NodeID: nodeB, Name: "langrishe"}))
	if r.Err != nil || r.Events[0].Kind != "pending withdrawn" || len(m.View().Pendings) != 0 {
		t.Fatalf("withdraw %+v", r)
	}
}

// Apply depends on nothing but the entries: the same log gives the same
// snapshot, whatever the map order.
func TestApplyIsDeterministic(t *testing.T) {
	var first []byte
	for run := 0; run < 20; run++ {
		m := New()
		m.Apply(1, initEntry(t, "i"))
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
			m.Apply(uint64(2+i), pendingEntry(t, fmt.Sprintf("p%d", i), id, fmt.Sprintf("n%d", i),
				fmt.Sprintf("10.0.1.%d:8444", i+1), pin(byte('0'+i)), 1000))
		}
		// A duplicate whose error names the conflict: the message must
		// be the same every time.
		r := m.Apply(20, pendingEntry(t, "dup", nodeC, "n3", "10.0.0.3:8444", pinC, 1000))
		snap, _ := m.Snapshot()
		got := append(snap, []byte(r.Err.Error())...)
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Fatalf("run %d differs", run)
		}
	}
}

func TestMembershipWriterRunsFirstAndStopsOnFailure(t *testing.T) {
	m := New()
	var written []Membership
	fail := false
	m.SetMembershipWriter(func(rec Membership) error {
		// The index is not reported as applied before the write.
		if applied, _ := m.appliedUnlocked(); applied >= rec.Index {
			t.Errorf("index %d reported before the file was written", rec.Index)
		}
		if fail {
			return errors.New("disk full")
		}
		written = append(written, rec)
		return nil
	})
	m.Apply(1, initEntry(t, "i"))
	m.Apply(2, pendingEntry(t, "p", nodeB, "langrishe", "10.0.0.2:8444", pinB, 1000))
	if len(written) != 1 || written[0].Index != 1 || written[0].Epoch != 1 {
		t.Fatalf("written %+v (a pending entry does not change the membership file)", written)
	}
	fail = true
	r := m.Apply(3, joinEntry(t, "j", nodeB, pinB, 10))
	if !errors.Is(r.Err, ErrStalled) {
		t.Fatalf("a failed write did not stop the machine: %+v", r)
	}
	v := m.View()
	if v.AppliedIndex != 2 || v.Stalled == nil || v.Stalled.Index != 3 || len(v.Pendings) != 1 {
		t.Fatalf("after a failed write: %+v", v)
	}
	if _, ok := m.Member(nodeB); ok {
		t.Fatal("the entry whose file write failed was applied")
	}
}

// appliedUnlocked reads the applied index from inside the writer, which
// runs with the machine's lock held.
func (m *Machine) appliedUnlocked() (uint64, bool) { return m.applied, m.stalled != nil }

func TestSnapshotRestore(t *testing.T) {
	m := machineWithPending(t)
	m.Apply(3, joinEntry(t, "j", nodeB, pinB, 10))
	b, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	n := New()
	var rec []Membership
	n.SetMembershipWriter(func(r Membership) error { rec = append(rec, r); return nil })
	notified := false
	n.SetOnApplied(func() { notified = true })
	if err := n.Restore(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	got, want := n.View(), m.View()
	if got.ClusterID != want.ClusterID || len(got.Members) != 2 || got.Members[1] != want.Members[1] ||
		got.AppliedIndex != 3 || got.MembershipIndex != 3 || got.Epoch != 1 {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
	// A restore changes the membership too: the file is written, at the
	// snapshot's membership index, and the listener is told.
	if len(rec) != 1 || rec[0].Index != 3 || len(rec[0].Members) != 2 || !notified {
		t.Fatalf("membership written at restore: %+v, notified %v", rec, notified)
	}
	// Operation IDs survive: a replayed entry is still a no-op.
	if r := n.Apply(4, joinEntry(t, "j", nodeB, pinB, 10)); !r.Duplicate {
		t.Fatal("the restored machine applied an old operation again")
	}
	if err := n.Restore(bytes.NewReader([]byte(`{"format":3}`))); err == nil {
		t.Fatal("a snapshot of an unknown format was restored")
	}
	// A restore whose file write fails changes nothing.
	n2 := New()
	n2.SetMembershipWriter(func(Membership) error { return errors.New("disk full") })
	if err := n2.Restore(bytes.NewReader(b)); err == nil || n2.View().ClusterID != "" {
		t.Fatal("a restore went on after a failed file write")
	}
}

// An 8a snapshot (format 1) is still read: epoch 1, no pending entries.
func TestRestoreFormat1(t *testing.T) {
	old := fmt.Sprintf(`{"format":1,"cluster_id":%q,"members":[{"id":%q,"name":"smith","address":"10.79.20.81:8444","pin":%q,"state":"member"}],"ops":{"a":1},"applied":4}`,
		clusterA, nodeA, pinA)
	n := New()
	if err := n.Restore(strings.NewReader(old)); err != nil {
		t.Fatal(err)
	}
	if v := n.View(); v.Epoch != 1 || v.AppliedIndex != 4 || v.MembershipIndex != 4 || len(v.Members) != 1 {
		t.Fatalf("view %+v", v)
	}
}

// A stalled machine takes no snapshot, so the library never truncates the
// log past the entry it stopped at.
func TestStalledMachineRefusesASnapshot(t *testing.T) {
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	m.Apply(2, []byte(`{"type":"user.create","version":1,"op_id":"x","actor":"a","origin":"n","data":{}}`))
	if _, err := m.Snapshot(); !errors.Is(err, ErrSnapshotWhileStalled) {
		t.Fatalf("snapshot of a stalled machine: %v", err)
	}
}

// A snapshot written by the first 8a build may carry the stall; it is read
// and ignored, so the restored machine applies again.
func TestRestoreIgnoresAStallInAnOldSnapshot(t *testing.T) {
	old := fmt.Sprintf(`{"format":1,"cluster_id":%q,"members":[{"id":%q,"name":"smith","address":"10.0.0.1:8444","pin":%q,"state":"member"}],"ops":{},"applied":1,"stalled":{"index":2,"reason":"old build"}}`,
		clusterA, nodeA, pinA)
	n := New()
	if err := n.Restore(strings.NewReader(old)); err != nil {
		t.Fatal(err)
	}
	if v := n.View(); v.Stalled != nil || v.AppliedIndex != 1 {
		t.Fatalf("restored view %+v", v)
	}
	if r := n.Apply(2, addressEntry(t, "op-addr", "10.0.0.2:8444")); r.Err != nil {
		t.Fatalf("apply after the restore: %+v", r)
	}
	b, _ := n.Snapshot()
	if bytes.Contains(b, []byte("stalled")) {
		t.Fatalf("a snapshot carries the stall: %s", b)
	}
}

// Every type of this build has its own apply step: with empty data each is
// refused (none is a silent no-op).
func TestEveryTypeHasAnApplyStep(t *testing.T) {
	for typ := range Supported {
		m := machineWithPending(t)
		r := m.Apply(3, encode(t, typ, "empty-"+typ, struct{}{}))
		if r.Err == nil {
			t.Errorf("%s with empty data was applied without a check", typ)
		}
	}
}
