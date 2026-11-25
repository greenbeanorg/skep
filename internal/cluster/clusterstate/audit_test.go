package clusterstate

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type fakeAudit struct {
	entries []AuditEntry
	snaps   []uint64
	fail    error
}

func (f *fakeAudit) AuditEntry(a AuditEntry) error {
	if f.fail != nil {
		return f.fail
	}
	f.entries = append(f.entries, a)
	return nil
}

func (f *fakeAudit) AuditSnapshot(_, index uint64, _ []string) error {
	if f.fail != nil {
		return f.fail
	}
	f.snaps = append(f.snaps, index)
	return nil
}

// D4: every entry the machine handles is given to the audit before its
// index is reported as applied: applied, refused (with the reason) or a
// duplicate, with what it names. A failed audit write stops the machine at
// that entry, as a failed write of the applied-membership file does.
func TestAuditBeforeApplied(t *testing.T) {
	m := New()
	a := &fakeAudit{}
	m.SetAuditWriter(a)
	m.Apply(1, initEntry(t, "op-init"))
	m.Apply(2, pendingEntry(t, "op-p", nodeB, "langrishe", "10.0.0.2:8444", pinB, 100))
	m.Apply(3, pendingEntry(t, "op-p", nodeB, "langrishe", "10.0.0.2:8444", pinB, 100)) // duplicate
	m.Apply(4, pendingEntry(t, "op-q", nodeC, "langrishe", "10.0.0.3:8444", pinC, 100)) // refused: the name
	if len(a.entries) != 4 {
		t.Fatalf("%d records", len(a.entries))
	}
	got := []string{}
	for _, e := range a.entries {
		got = append(got, e.Outcome)
	}
	if strings.Join(got, ",") != "applied,applied,duplicate,refused" || a.entries[3].Reason == "" {
		t.Fatalf("outcomes %v %+v", got, a.entries[3])
	}
	if e := a.entries[0]; e.Epoch != 1 || e.Index != 1 || e.NodeID != nodeA || e.Name != "smith" || e.Pins[0] != pinA {
		t.Fatalf("init record %+v", e)
	}
	if e := a.entries[1]; e.Type != TypePendingCreate || e.NodeID != nodeB || e.Address != "10.0.0.2:8444" || e.OpID != "op-p" || e.Actor != ActorSystem {
		t.Fatalf("pending record %+v", e)
	}
	a.fail = errors.New("disk full")
	res := m.Apply(5, joinEntry(t, "op-j", nodeB, pinB, 50))
	if !errors.Is(res.Err, ErrStalled) || !strings.Contains(res.Err.Error(), "cannot write the cluster audit") {
		t.Fatalf("a failed audit write: %v", res.Err)
	}
	if applied, stalled := m.AppliedIndex(); applied != 4 || !stalled {
		t.Fatalf("applied %d stalled %v", applied, stalled)
	}
	if _, ok := m.Member(nodeB); ok {
		t.Fatal("the entry was applied although its record was not written")
	}
	// A restore with a failing audit keeps the old state.
	snap, err := New().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	m2 := New()
	m2.Apply(1, initEntry(t, "op-init"))
	full, _ := m2.Snapshot()
	m3 := New()
	m3.SetAuditWriter(a)
	if err := m3.Restore(bytes.NewReader(full)); err == nil || !strings.Contains(err.Error(), "cluster audit") {
		t.Fatalf("restore with a failing audit: %v", err)
	}
	a.fail = nil
	if err := m3.Restore(bytes.NewReader(full)); err != nil || len(a.snaps) != 1 || a.snaps[0] != 1 {
		t.Fatalf("restore: %v %v", err, a.snaps)
	}
	_ = snap
}
