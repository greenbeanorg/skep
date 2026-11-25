package clusterstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func keyAddEntry(t *testing.T, op, id, next string) []byte {
	return encode(t, TypeMemberKeyAdd, op, MemberKeyAdd{NodeID: id, NextPin: next, At: 50})
}

func keyDoneEntry(t *testing.T, id, keep string, add uint64) []byte {
	return encode(t, TypeMemberKeyDone, KeyDoneOpID(id, add, keep), MemberKeyDone{NodeID: id, Keep: keep, AddIndex: add})
}

func refused(t *testing.T, r Result, want string) {
	t.Helper()
	if !errors.Is(r.Err, ErrRefused) || !strings.Contains(r.Err.Error(), want) {
		t.Fatalf("want a refusal with %q, got %v", want, r.Err)
	}
}

// D1: member.key_add and member.key_done, their refusals, and what they
// change.
func TestKeyRotationEntries(t *testing.T) {
	pinN := pin('e')
	m := threeMembers(t) // C is joining
	refused(t, m.Apply(10, keyAddEntry(t, "k0", nodeB, pinN)), "is joining")
	m.Apply(11, promotedEntry(t, "prc", nodeC))
	refused(t, m.Apply(12, keyAddEntry(t, "k1", "3e8b6d85-0c3b-4f8b-a03e-6c4f9c3d2f44", pinN)), "not a member")
	refused(t, m.Apply(13, keyAddEntry(t, "k2", nodeB, pinA)), "belongs to member smith")
	refused(t, m.Apply(14, keyAddEntry(t, "k3", nodeB, "sha256:x")), "not a key pin")
	// An admission pending refuses a rotation.
	m.Apply(15, pendingEntry(t, "pd", "4f9c7e96-1d4c-4a9c-b14f-7d5a0d4e3a55", "d", "10.0.0.4:8444", pin('d'), 100))
	refused(t, m.Apply(16, keyAddEntry(t, "k4", nodeB, pinN)), "admission is pending")
	m.Apply(17, encode(t, TypePendingWithdraw, "wd", PendingWithdraw{NodeID: "4f9c7e96-1d4c-4a9c-b14f-7d5a0d4e3a55", Name: "d"}))
	// The rotation begins.
	if r := m.Apply(18, keyAddEntry(t, "k5", nodeB, pinN)); r.Err != nil {
		t.Fatal(r.Err)
	}
	b, _ := m.Member(nodeB)
	if b.Pin != pinB || b.NextPin != pinN || b.KeyAddIndex != 18 {
		t.Fatalf("%+v", b)
	}
	// One at a time; a duplicate of the same operation changes nothing.
	refused(t, m.Apply(19, keyAddEntry(t, "k6", nodeC, pin('f'))), "one rotation in the cluster at a time")
	if r := m.Apply(20, keyAddEntry(t, "k5", nodeB, pinN)); !r.Duplicate {
		t.Fatal("not a duplicate")
	}
	// No admission while a member rotates; the pin of the next key is
	// taken.
	refused(t, m.Apply(21, pendingEntry(t, "pe", "5a0d8fa7-2e5d-4bad-825a-8e6b1e5f4b66", "e", "10.0.0.5:8444", pin('7'), 100)), "rotating its key")
	// A snapshot with a next pin is format 4 and restores it.
	snap, _ := m.Snapshot()
	var hdr struct {
		Format int `json:"format"`
	}
	_ = json.Unmarshal(snap, &hdr)
	if hdr.Format != 4 {
		t.Fatalf("format %d", hdr.Format)
	}
	n := New()
	if err := n.Restore(bytes.NewReader(snap)); err != nil {
		t.Fatal(err)
	}
	if nb, _ := n.Member(nodeB); nb.NextPin != pinN || nb.KeyAddIndex != 18 {
		t.Fatalf("restored %+v", nb)
	}
	// key_done: an earlier rotation's entry does not end this one.
	refused(t, m.Apply(22, keyDoneEntry(t, nodeB, KeepNew, 7)), "began at index 18")
	refused(t, m.Apply(23, keyDoneEntry(t, nodeC, KeepNew, 18)), "not rotating")
	refused(t, m.Apply(24, encode(t, TypeMemberKeyDone, "kd-x", MemberKeyDone{NodeID: nodeB, Keep: "both", AddIndex: 18})), "neither")
	if r := m.Apply(25, keyDoneEntry(t, nodeB, KeepNew, 18)); r.Err != nil {
		t.Fatal(r.Err)
	}
	if b, _ = m.Member(nodeB); b.Pin != pinN || b.NextPin != "" || b.KeyAddIndex != 0 {
		t.Fatalf("after done: %+v", b)
	}
	// Once one of the two is applied, the other is refused.
	refused(t, m.Apply(26, keyDoneEntry(t, nodeB, KeepOld, 18)), "not rotating")
	if snap, _ = m.Snapshot(); bytes.Contains(snap, []byte(`"format":4`)) {
		t.Fatal("format 4 without a next pin")
	}
	// keep old leaves the pin.
	m.Apply(27, keyAddEntry(t, "k7", nodeC, pin('f')))
	m.Apply(28, keyDoneEntry(t, nodeC, KeepOld, 27))
	if c, _ := m.Member(nodeC); c.Pin != pinC || c.NextPin != "" {
		t.Fatalf("keep old: %+v", c)
	}
	// A removing member cannot start one; a removal during a rotation is
	// allowed and takes both pins.
	m.Apply(29, keyAddEntry(t, "k8", nodeB, pin('9')))
	if r := m.Apply(30, removingEntry(t, "rb", nodeB)); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := m.Apply(31, removedEntry(t, "rdb", nodeB)); r.Err != nil {
		t.Fatal(r.Err)
	}
	if _, ok := m.Member(nodeB); ok {
		t.Fatal("still a member")
	}
	m.Apply(32, removingEntry(t, "rc", nodeC))
	refused(t, m.Apply(33, keyAddEntry(t, "k9", nodeC, pin('8'))), "not member")
}
