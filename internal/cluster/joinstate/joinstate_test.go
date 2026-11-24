package joinstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/joincode"
)

func TestMoveAsideAndPutBack(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "raft.db"), []byte("db"), 0o600)
	os.Mkdir(filepath.Join(dir, "snapshots"), 0o700)
	os.WriteFile(filepath.Join(dir, "node.key"), []byte("k"), 0o600)
	names := []string{"raft.db", "snapshots", "membership.json"}
	aside, err := MoveAside(dir, names, "20261124T000000Z")
	if err != nil || aside != "standalone.20261124T000000Z" {
		t.Fatal(aside, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "raft.db")); err == nil {
		t.Fatal("raft.db not moved")
	}
	if _, err := os.Stat(filepath.Join(dir, "node.key")); err != nil {
		t.Fatal("the key was moved")
	}
	info, _ := os.Stat(filepath.Join(dir, aside))
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", info.Mode())
	}
	// Putting back refuses when a store exists in its place.
	os.WriteFile(filepath.Join(dir, "raft.db"), []byte("new"), 0o600)
	if err := PutBack(dir, aside, names); err == nil {
		t.Fatal("put back over a new store")
	}
	os.Remove(filepath.Join(dir, "raft.db"))
	if err := PutBack(dir, aside, names); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "raft.db")); string(b) != "db" {
		t.Fatal("not the old store")
	}
	if _, err := os.Stat(filepath.Join(dir, aside)); err == nil {
		t.Fatal("the moved-aside directory is left")
	}
	for _, bad := range []string{"../x", "raft.db", "standalone."} {
		if err := PutBack(dir, bad, names); err == nil {
			t.Errorf("PutBack(%q)", bad)
		}
		if err := DeleteStandalone(dir, bad); err == nil {
			t.Errorf("DeleteStandalone(%q)", bad)
		}
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
