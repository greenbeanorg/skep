package clusterstate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func apiKeyEntry(t *testing.T, op, id, pin string) []byte {
	return encode(t, TypeMemberAPIKey, op, MemberAPIKey{NodeID: id, APIPin: pin, At: 77})
}

func apiPin(c string) string { return "sha256:" + strings.Repeat(c, 64) }

const nodeD = "dddddddd-0000-4000-8000-00000000000d"

var pinD = apiPin("d")

// twoMachines builds the same state on two machines: A and B members, C
// joining, and a pending entry for D (step 8e-1, D46: every refusal on two
// machines with the same result and text).
func twoMachines(t *testing.T) [2]*Machine {
	var out [2]*Machine
	for i := range out {
		m := threeMembers(t)
		if r := m.Apply(7, pendingEntry(t, "pd", nodeD, "blazanov", "10.0.0.4:8444", pinD, 100)); r.Err != nil {
			t.Fatal(r.Err)
		}
		if r := m.Apply(8, apiKeyEntry(t, "kb", nodeB, apiPin("e"))); r.Err != nil {
			t.Fatal(r.Err)
		}
		out[i] = m
	}
	return out
}

func TestAPIKeyRefusalsAreTheSameOnTwoMachines(t *testing.T) {
	unknown := "99999999-0000-4000-8000-000000000099"
	cases := []struct {
		name, id, pin, code, text string
	}{
		{"not a member", unknown, apiPin("f"), CodeNotFound, "is not a member"},
		{"joining", nodeC, apiPin("f"), CodeInvalid, "in state joining"},
		{"not a pin", nodeA, "sha256:xyz", CodeInvalid, "is not a key pin"},
		{"its own node pin", nodeA, pinA, CodeInvalid, "its own node key's pin"},
		{"another member's node pin", nodeA, pinB, CodeInvalid, "belongs to member langrishe"},
		{"another member's API pin", nodeA, apiPin("e"), CodeInvalid, "belongs to member langrishe"},
		{"a joining member's node pin", nodeA, pinC, CodeInvalid, "belongs to member star"},
		{"a pending entry's pin", nodeA, pinD, CodeInvalid, "pending entry of blazanov"},
	}
	for i, c := range cases {
		ms := twoMachines(t)
		var texts [2]string
		for j, m := range ms {
			before, _ := m.Snapshot()
			r := m.Apply(9, apiKeyEntry(t, "x"+string(rune('a'+i)), c.id, c.pin))
			if r.Err == nil || RefusalCode(r.Err) != c.code || !strings.Contains(r.Err.Error(), c.text) {
				t.Fatalf("%s: %v (code %q)", c.name, r.Err, RefusalCode(r.Err))
			}
			after, _ := m.Snapshot()
			// Nothing changed but the applied index.
			var b, a snapshot
			_ = json.Unmarshal(before, &b)
			_ = json.Unmarshal(after, &a)
			if len(a.Members) != len(b.Members) || a.Members[0] != b.Members[0] || a.Applied != 9 {
				t.Fatalf("%s: state changed", c.name)
			}
			texts[j] = r.Err.Error()
		}
		if texts[0] != texts[1] {
			t.Fatalf("%s: two texts: %q %q", c.name, texts[0], texts[1])
		}
	}
	// A removing member is refused too.
	m := threeMembers(t)
	if r := m.Apply(7, promotedEntry(t, "prc", nodeC)); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := m.Apply(8, removingEntry(t, "rmc", nodeC)); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := m.Apply(9, apiKeyEntry(t, "kc", nodeC, apiPin("f"))); RefusalCode(r.Err) != CodeInvalid || !strings.Contains(r.Err.Error(), "in state removing") {
		t.Fatalf("removing: %v", r.Err)
	}
}

func TestAPIKeySetsReplacesAndIsUnchanged(t *testing.T) {
	m := threeMembers(t)
	var written []Membership
	m.SetMembershipWriter(func(ms Membership) error { written = append(written, ms); return nil })
	hooks := 0
	m.SetOnApplied(func() { hooks++ })
	r := m.Apply(7, apiKeyEntry(t, "k1", nodeA, apiPin("e")))
	if r.Err != nil || r.Unchanged || len(r.Events) != 1 {
		t.Fatalf("set: %+v", r)
	}
	if mem, _ := m.Member(nodeA); mem.APIPin != apiPin("e") {
		t.Fatalf("API pin %q", mem.APIPin)
	}
	// A membership change: the file first, then the hook.
	if len(written) != 1 || written[0].Index != 7 || written[0].Members[0].APIPin != apiPin("e") || hooks != 1 || m.View().MembershipIndex != 7 {
		t.Fatalf("written %+v, hooks %d", written, hooks)
	}
	// The same pin again (another operation ID): a success that changes
	// nothing; its operation ID is recorded.
	r = m.Apply(8, apiKeyEntry(t, "k2", nodeA, apiPin("e")))
	if r.Err != nil || !r.Unchanged || len(written) != 1 || hooks != 1 || m.View().MembershipIndex != 7 {
		t.Fatalf("unchanged: %+v, written %d, hooks %d", r, len(written), hooks)
	}
	if _, ok := m.OpIndex("k2"); !ok {
		t.Fatal("the unchanged entry's operation ID was not recorded")
	}
	// A new pin replaces the earlier one.
	if r := m.Apply(9, apiKeyEntry(t, "k3", nodeA, apiPin("f"))); r.Err != nil {
		t.Fatal(r.Err)
	}
	if mem, _ := m.Member(nodeA); mem.APIPin != apiPin("f") || len(written) != 2 {
		t.Fatalf("replace: %+v", mem)
	}
	// Allowed while a member rotates and while an admission is pending.
	m2 := threeMembers(t)
	for i, b := range [][]byte{promotedEntry(t, "prc", nodeC), keyAddEntry(t, "ka", nodeB, apiPin("1")), apiKeyEntry(t, "kr", nodeA, apiPin("e"))} {
		if r := m2.Apply(uint64(7+i), b); r.Err != nil {
			t.Fatalf("during a rotation, step %d: %v", i, r.Err)
		}
	}
	m3 := machineWithPending(t)
	if r := m3.Apply(3, apiKeyEntry(t, "kp", nodeA, apiPin("e"))); r.Err != nil {
		t.Fatalf("while pending: %v", r.Err)
	}
	// A rotating member's next pin cannot be another's API pin.
	if r := m2.Apply(10, apiKeyEntry(t, "kn", nodeA, apiPin("1"))); RefusalCode(r.Err) != CodeInvalid {
		t.Fatalf("next pin: %v", r.Err)
	}
}

// D4: no other entry sets an API pin.
func TestNoOtherEntrySetsAnAPIPin(t *testing.T) {
	m := New()
	r := m.Apply(1, encode(t, TypeClusterInit, "i", ClusterInit{ClusterID: clusterA,
		Member: Member{ID: nodeA, Name: "smith", Address: "10.0.0.1:8444", Pin: pinA, APIPin: apiPin("e"), State: StateMember}}))
	if r.Err == nil || !strings.Contains(r.Err.Error(), "an API pin is not accepted before step 8e") {
		t.Fatalf("init with an API pin: %v", r.Err)
	}
}

// D5: format 7 only once a member has an API pin; it restores.
func TestSnapshotFormatSevenOnlyWithAnAPIPin(t *testing.T) {
	m := threeMembers(t)
	b, _ := m.Snapshot()
	var s snapshot
	_ = json.Unmarshal(b, &s)
	if s.Format != 2 {
		t.Fatalf("format %d without an API pin", s.Format)
	}
	m.Apply(7, apiKeyEntry(t, "k1", nodeA, apiPin("e")))
	b, _ = m.Snapshot()
	_ = json.Unmarshal(b, &s)
	if s.Format != 7 {
		t.Fatalf("format %d with an API pin", s.Format)
	}
	n := New()
	if err := n.Restore(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if mem, _ := n.Member(nodeA); mem.APIPin != apiPin("e") {
		t.Fatalf("restored %+v", mem)
	}
	again, _ := n.Snapshot()
	if !bytes.Equal(again, b) {
		t.Fatalf("snapshot after restore differs:\n%s\n%s", again, b)
	}
}

// D6: the audit entry names the node and its name, and the API pin in its
// own field.
func TestAPIKeyAudit(t *testing.T) {
	m := threeMembers(t)
	var got []AuditEntry
	m.SetAuditWriter(recorder(func(a AuditEntry) { got = append(got, a) }))
	m.Apply(7, apiKeyEntry(t, "k1", nodeA, apiPin("e")))
	if len(got) != 1 || got[0].NodeID != nodeA || got[0].Name != "smith" || got[0].APIPin != apiPin("e") || len(got[0].Pins) != 0 || got[0].At != 77 {
		t.Fatalf("audit %+v", got)
	}
}

type recorder func(AuditEntry)

func (f recorder) AuditEntry(a AuditEntry) error              { f(a); return nil }
func (recorder) AuditSnapshot(uint64, uint64, []Member) error { return nil }

func TestGuestByID(t *testing.T) {
	m := threeMembers(t)
	id := "33333333-0000-4000-8000-000000000003"
	if r := m.Apply(7, encode(t, TypeGuestAdd, "ga", GuestAdd{ID: id, Kind: KindVM, Name: "db", Owner: nodeB, At: 1})); r.Err != nil {
		t.Fatal(r.Err)
	}
	if g, ok := m.GuestByID(id); !ok || g.Owner != nodeB || g.Kind != KindVM {
		t.Fatalf("%+v %v", g, ok)
	}
	if _, ok := m.GuestByID("44444444-0000-4000-8000-000000000004"); ok {
		t.Fatal("found an unknown guest")
	}
}
