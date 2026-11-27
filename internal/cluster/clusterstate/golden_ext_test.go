package clusterstate_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	cs "github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

const goldenDir = "testdata/main-c60571a"

// goldenSteps are the entries testdata/main-c60571a/golden_gen_test.go.txt
// wrote with main's code, built here with this build's constructors.
func goldenSteps() []struct {
	typ, op string
	data    any
} {
	const (
		cid = "c1c1c1c1-0000-4000-8000-000000000001"
		a   = "a1a1a1a1-0000-4000-8000-00000000000a"
		b   = "b1b1b1b1-0000-4000-8000-00000000000b"
		d   = "d1d1d1d1-0000-4000-8000-00000000000d"
		g1  = "11111111-0000-4000-8000-000000000001"
		g2  = "22222222-0000-4000-8000-000000000002"
	)
	pin := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	return []struct {
		typ, op string
		data    any
	}{
		{cs.TypeClusterInit, "init", cs.ClusterInit{ClusterID: cid, Member: cs.Member{ID: a, Name: "smith", Address: "10.0.0.1:8444", Pin: pin("a"), State: cs.StateMember}}},
		{cs.TypePendingCreate, "p-b", cs.PendingCreate{NodeID: b, Name: "langrishe", Address: "10.0.0.2:8444", Pin: pin("b"), ExpiresAt: 2000}},
		{cs.TypeMemberJoin, "join:b", cs.MemberJoin{NodeID: b, Pin: pin("b"), At: 1000}},
		{cs.TypeMemberPromoted, "promote:b", cs.MemberPromoted{NodeID: b}},
		{cs.TypeGuestReserve, "g-r1", cs.GuestReserve{Kind: "container", Name: "web1", Key: "k1", Owner: b, At: 1001}},
		{cs.TypeGuestAdd, "g-a1", cs.GuestAdd{ID: g1, Kind: "container", Name: "web1", Owner: b, At: 1002}},
		{cs.TypeGuestReserve, "g-r2", cs.GuestReserve{Kind: "vm", Name: "db1", Key: "k2", Owner: a, At: 1003}},
		{cs.TypeGuestAdd, "g-a2", cs.GuestAdd{ID: g2, Kind: "vm", Name: "db2", Owner: a, At: 1004}},
		{cs.TypePendingCreate, "p-d", cs.PendingCreate{NodeID: d, Name: "star", Address: "10.0.0.3:8444", Pin: pin("d"), ExpiresAt: 3000}},
		{cs.TypeMemberJoin, "join:d", cs.MemberJoin{NodeID: d, Pin: pin("d"), At: 1005}},
		{cs.TypeMemberRemoving, "rm-d", cs.MemberRemoving{NodeID: d, ForgetGuests: []string{}}},
		{cs.TypeMemberRemoved, "rmd-d", cs.MemberRemoved{NodeID: d}},
		{cs.TypeGuestRemove, "g-x2", cs.GuestRemove{ID: g2, Owner: a}},
		{cs.TypeMemberRemoving, "rm-b-1", cs.MemberRemoving{NodeID: b, ForgetGuests: []string{}}},
		{cs.TypeGuestRelease, "g-l2", cs.GuestRelease{Kind: "vm", Name: "db1", Key: "k2", Owner: a}},
		{cs.TypeGuestRemove, "g-x1", cs.GuestRemove{ID: g1, Owner: b}},
		{cs.TypeMemberRemoving, "rm-b-2", cs.MemberRemoving{NodeID: b, ForgetGuests: []string{}}},
		{cs.TypeMemberRemoved, "rmd-b", cs.MemberRemoved{NodeID: b}},
	}
}

// normalized drops what depends on the moment a record was written.
func normalized(t *testing.T, recs []audit.Record) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, r := range recs {
		r.LocalTime, r.PrevHash, r.Hash = "", "", ""
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(append(b, '\n'))
	}
	return out.Bytes()
}

// D27, D1: the version 1 log main wrote (testdata/main-c60571a) applies
// with this build to main's state and the same audit records, and this
// build's constructors write the same log bytes: a cluster without guests
// writes and applies the same log as before step 8d-2.
func TestMainLogAppliesAsBefore(t *testing.T) {
	logBytes, err := os.ReadFile(filepath.Join(goldenDir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	wantState, _ := os.ReadFile(filepath.Join(goldenDir, "state.json"))
	dir := t.TempDir()
	if err := os.MkdirAll(audit.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := audit.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := cs.New()
	m.SetAuditWriter(l)
	var mine bytes.Buffer
	steps := goldenSteps()
	sc := bufio.NewScanner(bytes.NewReader(logBytes))
	n := 0
	for sc.Scan() {
		var line struct {
			Index uint64          `json:"index"`
			Entry json.RawMessage `json:"entry"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		m.Apply(line.Index, line.Entry)
		s := steps[n]
		e, err := cs.NewEntry(s.typ, s.op, "root", "a1a1a1a1-0000-4000-8000-00000000000a", s.data)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := e.Encode()
		lb, _ := json.Marshal(struct {
			Index uint64          `json:"index"`
			Entry json.RawMessage `json:"entry"`
		}{line.Index, raw})
		mine.Write(append(lb, '\n'))
		n++
	}
	if n != len(steps) {
		t.Fatalf("%d lines", n)
	}
	if !bytes.Equal(mine.Bytes(), logBytes) {
		t.Fatalf("this build writes another log:\n%s", mine.String())
	}
	if _, stalled := m.AppliedIndex(); stalled {
		t.Fatal("stalled")
	}
	got, err := m.Snapshot()
	if err != nil || !bytes.Equal(got, wantState) {
		t.Fatalf("state differs from main's:\n%s\n%s", got, wantState)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	mainRecs, err := audit.ReadAll(goldenAuditDir(t))
	if err != nil {
		t.Fatal(err)
	}
	ourRecs, err := audit.ReadAll(audit.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := normalized(t, ourRecs), normalized(t, mainRecs); !bytes.Equal(a, b) {
		t.Fatalf("the audit records differ from main's:\n%s\n%s", a, b)
	}
}

// goldenAuditDir copies main's audit segment into a directory of its own.
func goldenAuditDir(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(goldenDir, audit.SegmentName(1)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(audit.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(audit.Dir(root), audit.SegmentName(1)), src, 0o600); err != nil {
		t.Fatal(err)
	}
	return audit.Dir(root)
}

// V5: main's segment verifies with this build; records with the new
// fields appended to it verify too, and each new field is in the line.
func TestMainAuditVerifiesAndTakesTheNewFields(t *testing.T) {
	dir := goldenAuditDir(t)
	rep, err := audit.Verify(dir)
	if err != nil || rep.Break != nil || rep.Records != 18 {
		t.Fatalf("main's segment: %+v %v", rep, err)
	}
	l, err := audit.Open(filepath.Dir(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	g := []cs.JoinGuest{{ID: "33333333-0000-4000-8000-000000000003", Kind: "vm", Name: "<db&1>"}}
	for i, a := range []cs.AuditEntry{
		{Epoch: 1, Index: 19, Type: cs.TypeMemberJoin, Version: 2, OpID: "j2", Actor: "node:x", Origin: "o", Outcome: audit.OutcomeApplied, Guests: g},
		{Epoch: 1, Index: 20, Type: cs.TypeMemberRemoving, Version: 2, OpID: "r2", Actor: "root", Origin: "o", Outcome: audit.OutcomeApplied, GuestCount: 1, ReservationCount: 2},
		{Epoch: 1, Index: 21, Type: cs.TypeMemberRemoved, Version: 1, OpID: "d2", Actor: "system", Origin: "o", Outcome: audit.OutcomeApplied, GuestCount: 1},
	} {
		if err := l.AuditEntry(a); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err = audit.Verify(dir)
	if err != nil || rep.Break != nil || rep.Records != 21 {
		t.Fatalf("with the new fields: %+v %v", rep, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, audit.SegmentName(1)))
	for _, want := range []string{`"guests":[{"id":"33333333-0000-4000-8000-000000000003","kind":"vm","name":"\u003cdb\u00261\u003e"}]`,
		`"guest_count":1,"reservation_count":2`, `"index":21,"entry_type":"member.removed","entry_version":1,"actor":"system","op_id":"d2","origin":"o","guest_count":1,`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("the segment lacks %s", want)
		}
	}
	src, _ := os.ReadFile(filepath.Join(goldenDir, audit.SegmentName(1)))
	if !bytes.HasPrefix(b, src) {
		t.Fatal("main's lines were changed")
	}
}

// D6, V8 (step 8e-1): main's segment still verifies, and a member.api_key
// record appended to it verifies with its api_pin field.
func TestMainAuditTakesAnAPIPin(t *testing.T) {
	dir := goldenAuditDir(t)
	l, err := audit.Open(filepath.Dir(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	pin := "sha256:" + strings.Repeat("e", 64)
	if err := l.AuditEntry(cs.AuditEntry{Epoch: 1, Index: 19, Type: cs.TypeMemberAPIKey, Version: 1, OpID: "apikey:a:1", Actor: "node:a",
		Origin: "a", Outcome: audit.OutcomeApplied, NodeID: "a", Name: "smith", APIPin: pin, At: 5}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := audit.Verify(dir)
	if err != nil || rep.Break != nil || rep.Records != 19 {
		t.Fatalf("with an API pin: %+v %v", rep, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, audit.SegmentName(1)))
	if !bytes.Contains(b, []byte(`"api_pin":"`+pin+`"`)) {
		t.Fatal("the record lacks api_pin")
	}
	src, _ := os.ReadFile(filepath.Join(goldenDir, audit.SegmentName(1)))
	if !bytes.HasPrefix(b, src) {
		t.Fatal("main's lines were changed")
	}
}
