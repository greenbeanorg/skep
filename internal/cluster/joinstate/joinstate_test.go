package joinstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
)

// Moving aside is one rename of state/ and putting back one rename back;
// the key stays where it is; putting back refuses when state/ exists.
func TestMoveAsideAndPutBack(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state")
	os.Mkdir(st, 0o700)
	os.WriteFile(filepath.Join(st, "raft.db"), []byte("db"), 0o600)
	os.Mkdir(filepath.Join(st, "snapshots"), 0o700)
	os.WriteFile(filepath.Join(dir, "node.key"), []byte("k"), 0o600)
	aside, err := MoveAside(dir, "20261124T000000Z")
	if err != nil || aside != "standalone.20261124T000000Z" {
		t.Fatal(aside, err)
	}
	if _, err := os.Stat(st); err == nil {
		t.Fatal("state/ not moved")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, aside, "raft.db")); string(b) != "db" {
		t.Fatal("raft.db not inside the moved-aside directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "node.key")); err != nil {
		t.Fatal("the key was moved")
	}
	info, _ := os.Stat(filepath.Join(dir, aside))
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", info.Mode())
	}
	if _, err := MoveAside(dir, "20261124T000000Z"); err == nil {
		t.Fatal("a second move aside with the same name, without state/")
	}
	// Putting back refuses when a state exists in its place.
	os.Mkdir(st, 0o700)
	if err := PutBack(dir, aside); err == nil {
		t.Fatal("put back over a new state")
	}
	os.Remove(st)
	if err := PutBack(dir, aside); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(st, "raft.db")); string(b) != "db" {
		t.Fatal("not the old store")
	}
	if _, err := os.Stat(filepath.Join(dir, aside)); err == nil {
		t.Fatal("the moved-aside directory is left")
	}
	for _, bad := range []string{"../x", "raft.db", "standalone.", "state"} {
		if err := PutBack(dir, bad); err == nil {
			t.Errorf("PutBack(%q)", bad)
		}
		if err := DeleteStandalone(dir, bad); err == nil {
			t.Errorf("DeleteStandalone(%q)", bad)
		}
	}
}

// Once an answer is accepted it replaces the admission's list (D10);
// AllNodes is both, for join --abandon.
func TestPeersAnswerReplacesAdmission(t *testing.T) {
	a := joincode.Node{NodeID: "a", Name: "a"}
	b := joincode.Node{NodeID: "b", Name: "b"}
	c := joincode.Node{NodeID: "c", Name: "c"}
	self := joincode.Node{NodeID: "self", Name: "self"}
	s := State{Admission: joincode.Admission{Members: []joincode.Node{a, b}}}
	if p := s.Peers("self"); len(p) != 2 {
		t.Fatalf("before the answer: %v", p)
	}
	s.Answer = []joincode.Node{b, c, self}
	if p := s.Peers("self"); len(p) != 2 || p[0].NodeID != "b" || p[1].NodeID != "c" {
		t.Fatalf("after the answer: %v", p)
	}
	if all := s.AllNodes("self"); len(all) != 3 {
		t.Fatalf("all nodes: %v", all)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	adm := joincode.Admission{ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"}
	s := State{Phase: PhaseCalling, OpID: "op", Admission: adm, Standalone: "standalone.x"}
	if err := Write(dir, s); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, FileName))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	got, err := Read(dir)
	if err != nil || got.Phase != PhaseCalling || got.OpID != "op" {
		t.Fatal(got, err)
	}
	s.Phase = PhaseAbandoning
	if err := Write(dir, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(dir); got.Phase != PhaseAbandoning {
		t.Fatal("phase abandoning not kept")
	}
	if err := Write(dir, State{Phase: "maybe", OpID: "op", Admission: adm, Standalone: "s"}); err == nil {
		t.Fatal("unknown phase written")
	}
	if err := Write(dir, State{Phase: PhaseCalling, OpID: "op", Admission: adm, Standalone: "../x"}); err == nil {
		t.Fatal("a path written as the standalone directory")
	}
	Remove(dir)
	if s, err := Read(dir); s != nil || err != nil {
		t.Fatal(s, err)
	}
}

// D14 of step 8d-2: join.json records the guest list (omitted when empty);
// the largest list D4 allows, indented, is read back; a list that breaks
// the rules is neither written nor read.
func TestStateRecordsTheGuests(t *testing.T) {
	dir := t.TempDir()
	adm := joincode.Admission{ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"}
	s := State{Phase: PhaseCalling, OpID: "op", Admission: adm, Standalone: "standalone.x"}
	if err := Write(dir, s); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); strings.Contains(string(b), "guests") {
		t.Fatalf("an empty list is written: %s", b)
	}
	// 1000 guests whose names fill the byte limit.
	list := make([]clusterstate.JoinGuest, clusterstate.MaxJoinGuests)
	for i := range list {
		list[i] = clusterstate.JoinGuest{ID: fmt.Sprintf("0a0a0a0a-0000-4000-8000-%012d", i), Kind: clusterstate.KindVM, Name: fmt.Sprintf("%04d", i)}
	}
	base, _ := json.Marshal(list)
	extra := (clusterstate.MaxJoinGuestsBytes - len(base)) / len(list)
	for i := range list {
		list[i].Name += strings.Repeat("n", extra)
	}
	if b, _ := json.Marshal(list); len(b) > clusterstate.MaxJoinGuestsBytes || len(b) < clusterstate.MaxJoinGuestsBytes-1000 {
		t.Fatalf("the list is %d bytes", len(b))
	}
	s.Guests = list
	if err := Write(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil || len(got.Guests) != len(list) || got.Guests[999] != list[999] {
		t.Fatalf("read back: %v", err)
	}
	s.Guests = []clusterstate.JoinGuest{list[0], list[0]}
	if err := Write(dir, s); err == nil {
		t.Fatal("a list with an ID twice was written")
	}
	// A file that holds such a list (written by hand) is not read.
	b, _ := os.ReadFile(filepath.Join(dir, FileName))
	bad := strings.Replace(string(b), list[1].ID, list[0].ID, 1)
	os.WriteFile(filepath.Join(dir, FileName), []byte(bad), 0o600)
	if _, err := Read(dir); err == nil {
		t.Fatal("a list with an ID twice was read")
	}
}
