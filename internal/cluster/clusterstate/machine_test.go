package clusterstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

const (
	nodeA = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"
	pinA  = "sha256:aa"
)

func initEntry(t *testing.T, op string) []byte {
	t.Helper()
	e, err := NewEntry(TypeClusterInit, op, ActorSystem, nodeA, ClusterInit{
		ClusterID: "c1", Member: Member{ID: nodeA, Name: "smith", Pin: pinA, State: StateMember}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func addressEntry(t *testing.T, op, addr string) []byte {
	t.Helper()
	e, err := NewEntry(TypeMemberAddress, op, ActorSystem, nodeA, MemberAddress{NodeID: nodeA, Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.Encode()
	return b
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
	if id, ok := m.PinnedNode(pinA); !ok || id != nodeA {
		t.Fatal("the first member's key is not pinned")
	}
	if _, ok := m.PinnedNode("sha256:bb"); ok {
		t.Fatal("an unknown pin is accepted")
	}
	if _, ok := m.PinnedNode(""); ok {
		t.Fatal("an empty pin is accepted")
	}
	// A second cluster.init is refused and changes nothing.
	e, _ := NewEntry(TypeClusterInit, "op-init-2", ActorSystem, nodeA, ClusterInit{
		ClusterID: "c2", Member: Member{ID: "other", Name: "x", Pin: "sha256:bb", State: StateMember}})
	b, _ := e.Encode()
	if r := m.Apply(2, b); !errors.Is(r.Err, ErrRefused) {
		t.Fatalf("second init: %+v", r)
	}
	if v := m.View(); v.ClusterID != "c1" || len(v.Members) != 1 || v.Stalled != nil {
		t.Fatalf("state after a refused entry: %+v", v)
	}
	if r := m.Apply(3, addressEntry(t, "op-addr", "10.0.0.1:8444")); r.Err != nil {
		t.Fatal(r.Err)
	}
	// The same operation ID again is a no-op, even with other content.
	if r := m.Apply(4, addressEntry(t, "op-addr", "10.0.0.9:8444")); !r.Duplicate || r.Err != nil {
		t.Fatalf("duplicate: %+v", r)
	}
	if mem, _ := m.Member(nodeA); mem.Address != "10.0.0.1:8444" {
		t.Fatalf("address %q", mem.Address)
	}
	if m.View().AppliedIndex != 4 {
		t.Fatalf("applied index %d", m.View().AppliedIndex)
	}
}

func TestUnknownEntryStopsTheMachine(t *testing.T) {
	cases := map[string][]byte{
		"unknown type":   []byte(`{"type":"user.create","version":1,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"newer version":  []byte(`{"type":"member.address","version":2,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"no type":        []byte(`{"version":1,"op_id":"x","actor":"a","origin":"n","data":{}}`),
		"not JSON":       []byte(`garbage`),
		"unknown field":  []byte(`{"type":"member.address","version":1,"op_id":"x","actor":"a","origin":"n","data":{},"extra":1}`),
		"malformed data": []byte(`{"type":"member.address","version":1,"op_id":"x","actor":"a","origin":"n","data":{"nope":1}}`),
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
		if r := m.Apply(3, addressEntry(t, "op-addr", "10.0.0.1:8444")); !errors.Is(r.Err, ErrStalled) {
			t.Fatalf("%s: applied after the stall: %+v", name, r)
		}
		if mem, _ := m.Member(nodeA); mem.Address != "" {
			t.Fatalf("%s: an entry after the stall changed the state", name)
		}
		// A stalled machine vouches for no key.
		if _, ok := m.PinnedNode(pinA); ok {
			t.Fatalf("%s: a stalled machine still accepts a pin", name)
		}
	}
}

func TestSnapshotRestore(t *testing.T) {
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	m.Apply(2, addressEntry(t, "op-addr", "10.0.0.1:8444"))
	b, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	n := New()
	if err := n.Restore(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if got, want := n.View(), m.View(); got.ClusterID != want.ClusterID || len(got.Members) != 1 ||
		got.Members[0] != want.Members[0] || got.AppliedIndex != 2 {
		t.Fatalf("restored %+v, want %+v", got, want)
	}
	// Operation IDs survive: a replayed entry is still a no-op.
	if r := n.Apply(3, addressEntry(t, "op-addr", "10.0.0.2:8444")); !r.Duplicate {
		t.Fatal("the restored machine applied an old operation again")
	}
	if err := n.Restore(bytes.NewReader([]byte(`{"format":2}`))); err == nil {
		t.Fatal("a snapshot of an unknown format was restored")
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
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	b, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("stalled")) {
		t.Fatalf("a snapshot carries the stall: %s", b)
	}
	old := bytes.Replace(b, []byte(`"applied":1`), []byte(`"applied":1,"stalled":{"index":2,"reason":"old build"}`), 1)
	if bytes.Equal(old, b) {
		t.Fatal("test setup: no applied field to extend")
	}
	n := New()
	if err := n.Restore(bytes.NewReader(old)); err != nil {
		t.Fatal(err)
	}
	if v := n.View(); v.Stalled != nil || v.AppliedIndex != 1 {
		t.Fatalf("restored view %+v", v)
	}
	if r := n.Apply(2, addressEntry(t, "op-addr", "10.0.0.1:8444")); r.Err != nil {
		t.Fatalf("apply after the restore: %+v", r)
	}
}
