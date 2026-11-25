package audit

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func entry(index uint64, outcome string) clusterstate.AuditEntry {
	return clusterstate.AuditEntry{Epoch: 1, Index: index, Type: clusterstate.TypePendingCreate, Version: 1, OpID: "op", Actor: "root",
		Origin: "n1", Outcome: outcome, NodeID: "n2", Name: "star", Address: "10.0.0.3:8444", Pins: []string{"sha256:aa"}}
}

func mustOpen(t *testing.T, dataDir string, c *fakeClock) *Log {
	t.Helper()
	l, err := Open(dataDir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The chain across months and sizes, the keyed replay, a reopen that goes
// on with the same chain, and verify's summary.
func TestChainSegmentsAndReplay(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)}
	l := mustOpen(t, data, c)
	if !l.Empty() {
		t.Fatal("a new audit is not empty")
	}
	if err := l.Started(1, 5); err != nil {
		t.Fatal(err)
	}
	if err := l.Started(1, 9); err != nil { // not empty any more: nothing
		t.Fatal(err)
	}
	for i := uint64(3); i <= 7; i++ { // 3..5 are at or below the start mark
		if err := l.AuditEntry(entry(i, OutcomeApplied)); err != nil {
			t.Fatal(err)
		}
	}
	c.set(time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)) // a new month: a new segment
	if err := l.AuditEntry(entry(8, OutcomeRefused)); err != nil {
		t.Fatal(err)
	}
	if err := l.Event(EventListenSet, map[string]string{"address": "10.0.0.1:8444"}); err != nil {
		t.Fatal(err)
	}
	l.segLimit = 1 // every append now starts a segment
	if err := l.AuditSnapshot(1, 20, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.AuditEntry(entry(19, OutcomeApplied)); err != nil { // below the snapshot: nothing
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen: the end of the chain is read back; a replay writes nothing.
	l = mustOpen(t, data, c)
	if e, i := l.Highest(); e != 1 || i != 20 {
		t.Fatalf("highest %d %d", e, i)
	}
	if err := l.AuditEntry(entry(7, OutcomeApplied)); err != nil {
		t.Fatal(err)
	}
	if err := l.AuditEntry(entry(21, OutcomeDuplicate)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	rep, err := Verify(Dir(data))
	if err != nil || rep.Break != nil {
		t.Fatalf("verify: %+v %v", rep.Break, err)
	}
	// started, 6, 7, 8, event, snapshot, 21
	if rep.Records != 7 || rep.First.Kind != KindStarted || rep.Last.Index != 21 || rep.Last.Seq != 7 {
		t.Fatalf("records %d first %+v last %+v", rep.Records, rep.First, rep.Last)
	}
	if len(rep.Segments) != 3 || len(rep.Sealed()) != 2 || rep.Segments[0] != SegmentName(1) {
		t.Fatalf("segments %v", rep.Segments)
	}
	// The first record of a segment carries the last hash of the one
	// before (checked by Verify); the modes are the service account's.
	info, _ := os.Stat(Dir(data))
	if info.Mode().Perm() != DirMode {
		t.Fatalf("dir mode %o", info.Mode().Perm())
	}
	info, _ = os.Stat(filepath.Join(Dir(data), SegmentName(1)))
	if info.Mode().Perm() != FileMode {
		t.Fatalf("file mode %o", info.Mode().Perm())
	}
}

// One changed byte anywhere is found, with the file and the line.
func TestVerifyFindsAChangedByte(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	l := mustOpen(t, data, c)
	_ = l.Started(1, 1)
	for i := uint64(2); i <= 6; i++ {
		_ = l.AuditEntry(entry(i, OutcomeApplied))
	}
	l.Close()
	path := filepath.Join(Dir(data), SegmentName(1))
	orig, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(orig), "\n")
	for _, tc := range []struct {
		name string
		line int
		edit func(string) string
	}{
		{"a name", 3, func(s string) string { return strings.Replace(s, `"star"`, `"stas"`, 1) }},
		{"an index", 4, func(s string) string { return strings.Replace(s, `"index":4`, `"index":9`, 1) }},
		{"a hash", 2, func(s string) string { return strings.Replace(s, `"hash":"`, `"hash":"0`, 1)[:len(s)] + "\n" }},
		{"a space", 5, func(s string) string { return strings.Replace(s, `,"kind"`, `, "kind"`, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([]string(nil), lines...)
			changed[tc.line-1] = tc.edit(changed[tc.line-1])
			if err := os.WriteFile(path, []byte(strings.Join(changed, "")), FileMode); err != nil {
				t.Fatal(err)
			}
			rep, err := Verify(Dir(data))
			if err != nil || rep.Break == nil || rep.Break.File != SegmentName(1) || rep.Break.Line != tc.line {
				t.Fatalf("break %+v %v", rep.Break, err)
			}
			// A broken audit is not written to.
			if _, err := Open(data, c.now); err == nil || !strings.Contains(err.Error(), "broken") {
				t.Fatalf("open of a broken audit: %v", err)
			}
		})
	}
	// Removing a record breaks the chain at the next one.
	if err := os.WriteFile(path, []byte(strings.Join(append(append([]string(nil), lines[:2]...), lines[3:]...), "")), FileMode); err != nil {
		t.Fatal(err)
	}
	if rep, _ := Verify(Dir(data)); rep.Break == nil || rep.Break.Line != 3 {
		t.Fatalf("a removed record: %+v", rep.Break)
	}
}

// A missing segment, a gap, a lower index.
func TestVerifySegmentsAndOrder(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	l := mustOpen(t, data, c)
	l.segLimit = 1
	_ = l.Started(1, 1)
	_ = l.AuditEntry(entry(2, OutcomeApplied))
	_ = l.AuditEntry(entry(3, OutcomeApplied))
	l.Close()
	if err := os.Remove(filepath.Join(Dir(data), SegmentName(2))); err != nil {
		t.Fatal(err)
	}
	rep, _ := Verify(Dir(data))
	if rep.Break == nil || !strings.Contains(rep.Break.Reason, SegmentName(2)+" is missing") {
		t.Fatalf("missing segment: %+v", rep.Break)
	}
}

// Verify on a missing directory creates nothing; Open creates it 0700.
func TestVerifyCreatesNothing(t *testing.T) {
	data := t.TempDir()
	rep, err := Verify(Dir(data))
	if err != nil || rep.Records != 0 || rep.Break != nil {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := os.Lstat(Dir(data)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verify created the directory: %v", err)
	}
}

// One writer: a second Open fails while the first holds the lock.
func TestOneWriter(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Now()}
	l := mustOpen(t, data, c)
	if _, err := Open(data, c.now); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second writer: %v", err)
	}
	l.Close()
	l2 := mustOpen(t, data, c)
	l2.Close()
}

// A cut-off last line (a crash during a write) is removed at the next
// open, and the repair is recorded; the chain goes on.
func TestTailRepair(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Now()}
	l := mustOpen(t, data, c)
	_ = l.Started(1, 1)
	_ = l.AuditEntry(entry(2, OutcomeApplied))
	l.Close()
	path := filepath.Join(Dir(data), SegmentName(1))
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, FileMode)
	_, _ = f.WriteString(`{"seq":3,"local_ti`)
	f.Close()
	if rep, _ := Verify(Dir(data)); rep.Break == nil || rep.Break.Line != 3 {
		t.Fatalf("cut-off line: %+v", rep.Break)
	}
	l = mustOpen(t, data, c)
	if err := l.AuditEntry(entry(3, OutcomeApplied)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	rep, _ := Verify(Dir(data))
	if rep.Break != nil || rep.Records != 4 {
		t.Fatalf("after the repair: %+v %d", rep.Break, rep.Records)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), EventTailRepaired) || !strings.Contains(string(b), `"bytes_removed":"18"`) {
		t.Fatalf("the repair is not recorded:\n%s", b)
	}
}

// A failed append returns the error and changes nothing.
func TestFailedAppend(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Now()}
	l := mustOpen(t, data, c)
	_ = l.Started(1, 1)
	l.failAppend = errors.New("disk full")
	if err := l.AuditEntry(entry(2, OutcomeApplied)); err == nil {
		t.Fatal("no error")
	}
	l.failAppend = nil
	if err := l.AuditEntry(entry(2, OutcomeApplied)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if rep, _ := Verify(Dir(data)); rep.Break != nil || rep.Records != 2 {
		t.Fatalf("%+v %d", rep.Break, rep.Records)
	}
}

// The key starts again at a reset mark; after a join's mark nothing is
// recorded until the node's own member.join, or a snapshot that lists it;
// the skip survives a reopen.
func TestResetMarksAndJoinSkip(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Now()}
	l := mustOpen(t, data, c)
	if err := l.Mark(EventInit, 1, 3, map[string]string{"cluster_id": "a"}); err != nil {
		t.Fatal(err)
	}
	_ = l.AuditEntry(entry(3, OutcomeApplied)) // the init's own entries: not again
	_ = l.AuditEntry(entry(9, OutcomeApplied))
	if err := l.Mark(EventJoinMovedAside, 0, 0, nil); err == nil {
		t.Fatal("a join mark without the node ID")
	}
	if err := l.Mark(EventJoinMovedAside, 0, 0, map[string]string{"node_id": "self"}); err == nil {
		t.Fatal("a join mark without the join's operation ID")
	}
	if err := l.Mark(EventJoinMovedAside, 0, 0, map[string]string{"node_id": "self", "join_op": "join:self:2"}); err != nil {
		t.Fatal(err)
	}
	_ = l.AuditEntry(entry(1, OutcomeApplied)) // the new cluster's history
	// An earlier join of the same node to this cluster is history too.
	old := entry(2, OutcomeApplied)
	old.Type, old.NodeID, old.OpID = clusterstate.TypeMemberJoin, "self", "join:self:1"
	_ = l.AuditEntry(old)
	_ = l.AuditSnapshot(1, 5, []clusterstate.Member{{ID: "other"}, {ID: "self", JoinOp: "join:self:1"}})
	l.Close()
	l = mustOpen(t, data, c) // the skip goes on after a reopen
	_ = l.AuditEntry(entry(6, OutcomeApplied))
	join := entry(7, OutcomeApplied)
	join.Type, join.NodeID, join.OpID = clusterstate.TypeMemberJoin, "self", "join:self:2"
	_ = l.AuditEntry(join)
	_ = l.AuditEntry(entry(8, OutcomeApplied))
	if err := l.Mark(EventJoinAbandoned, 1, 9, nil); err != nil { // back to the one-node log, last index 9
		t.Fatal(err)
	}
	_ = l.AuditEntry(entry(9, OutcomeApplied))
	_ = l.AuditEntry(entry(10, OutcomeApplied))
	l.Close()
	recs, _ := ReadAll(Dir(data))
	var got []string
	for _, r := range recs {
		got = append(got, r.Kind+":"+strconv.FormatUint(r.Index, 10))
	}
	want := "node.init:3 entry:9 node.join.started:0 entry:7 entry:8 node.join.abandoned:9 entry:10"
	if strings.Join(got, " ") != want {
		t.Fatalf("records\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if rep, _ := Verify(Dir(data)); rep.Break != nil {
		t.Fatalf("verify: %+v", rep.Break)
	}
	// A join skip ended by a snapshot that lists the node.
	data2 := t.TempDir()
	l = mustOpen(t, data2, c)
	_ = l.Mark(EventJoinMovedAside, 0, 0, map[string]string{"node_id": "self", "join_op": "join:self:9"})
	_ = l.AuditSnapshot(1, 40, []clusterstate.Member{{ID: "other"}, {ID: "self", JoinOp: "join:self:9"}})
	_ = l.AuditEntry(entry(41, OutcomeApplied))
	l.Close()
	if rep, _ := Verify(Dir(data2)); rep.Records != 3 || rep.Last.Index != 41 || rep.SkipNode != "" {
		t.Fatalf("%+v", rep)
	}
}
