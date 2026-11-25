package joinstate

import (
	"os"
	"path/filepath"
	"testing"

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
