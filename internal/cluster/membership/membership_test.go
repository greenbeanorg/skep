package membership

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

const (
	cid = "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"
	idA = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"
	idB = "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22"
)

func rec(epoch, index uint64, members ...string) clusterstate.Membership {
	m := clusterstate.Membership{ClusterID: cid, Epoch: epoch, Index: index}
	for i, id := range members {
		m.Members = append(m.Members, clusterstate.Member{ID: id, Name: "n" + id[:4], Address: "10.0.0." + string(rune('1'+i)) + ":8444",
			Pin: "sha256:" + strings.Repeat(string(rune('a'+i)), 64), State: clusterstate.StateMember})
	}
	return m
}

func TestWriteModeContentAndNeverBack(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil || w.Current() != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec(1, 5, idA, idB)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	f, err := Read(dir)
	if err != nil || f.Index != 5 || f.Epoch != 1 || len(f.Members) != 2 || f.ClusterID != cid {
		t.Fatalf("read %+v %v", f, err)
	}
	// A lower or equal (epoch, index) changes nothing, also after the
	// writer is made again (a restart).
	if err := w.Write(rec(1, 3, idA)); err != nil {
		t.Fatal(err)
	}
	w2, _ := NewWriter(dir)
	if err := w2.Write(rec(1, 5, idA)); err != nil {
		t.Fatal(err)
	}
	if f, _ := Read(dir); f.Index != 5 || len(f.Members) != 2 {
		t.Fatalf("the file went back: %+v", f)
	}
	// A higher epoch wins even at a lower index.
	if err := w2.Write(rec(2, 1, idA)); err != nil {
		t.Fatal(err)
	}
	if f, _ := Read(dir); f.Epoch != 2 || len(f.Members) != 1 {
		t.Fatalf("epoch 2: %+v", f)
	}
	// No temporary file is left.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
}

// A failure before the rename leaves the old file whole and is reported.
func TestFailedWriteLeavesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir)
	w.Write(rec(1, 5, idA))
	w.beforeRename = func() error { return errors.New("disk full") }
	if err := w.Write(rec(1, 6, idA, idB)); err == nil {
		t.Fatal("no error")
	}
	if f, _ := Read(dir); f.Index != 5 {
		t.Fatalf("file %+v", f)
	}
	if c := w.Current(); c.Index != 5 {
		t.Fatalf("writer thinks %d", c.Index)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary file left: %v", entries)
	}
}

func TestReadIsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"format":1,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[],"x":1}`,
		"pending entry": `{"format":1,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[],"pendings":[]}`,
		"bad pin":       `{"format":1,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[{"id":"` + idA + `","name":"a","address":"","pin":"x","state":"member"}]}`,
		"bad format":    `{"format":3,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[]}`,
		"2, no next":    `{"format":2,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[]}`,
		"next in 1":     `{"format":1,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[{"id":"` + idA + `","name":"a","address":"","pin":"sha256:` + strings.Repeat("a", 64) + `","next_pin":"sha256:` + strings.Repeat("b", 64) + `","state":"member"}]}`,
		"next = pin":    `{"format":2,"cluster_id":"` + cid + `","epoch":1,"index":1,"members":[{"id":"` + idA + `","name":"a","address":"","pin":"sha256:` + strings.Repeat("a", 64) + `","next_pin":"sha256:` + strings.Repeat("a", 64) + `","state":"member"}]}`,
		"no index":      `{"format":1,"cluster_id":"` + cid + `","epoch":1,"index":0,"members":[]}`,
		"too large":     strings.Repeat(" ", maxSize+1),
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600)
		if _, err := Read(dir); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// Step 8b-2b: a next pin is written in format 2, read back, and the file
// goes back to format 1 once no member has one.
func TestNextPinFormat(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	m := clusterstate.Membership{ClusterID: cid, Epoch: 1, Index: 5, Members: []clusterstate.Member{{ID: idA, Name: "a", Pin: pa, NextPin: pb, State: clusterstate.StateMember}}}
	if err := w.Write(m); err != nil {
		t.Fatal(err)
	}
	f, err := Read(dir)
	if err != nil || f.Format != 2 || f.Members[0].NextPin != pb {
		t.Fatalf("%+v %v", f, err)
	}
	m.Index, m.Members[0].Pin, m.Members[0].NextPin = 6, pb, ""
	if err := w.Write(m); err != nil {
		t.Fatal(err)
	}
	if f, err = Read(dir); err != nil || f.Format != 1 || f.Members[0].Pin != pb {
		t.Fatalf("%+v %v", f, err)
	}
}

// D5 (step 8e-1): an API pin is valid, never twice, never a node pin; the
// format numbers do not change.
func TestAPIPinValidation(t *testing.T) {
	pin := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	m := func(id, p, api string) Member {
		return Member{ID: id, Name: "n" + id[:1], Pin: p, APIPin: api, State: "member"}
	}
	a, b := "aaaaaaaa-0000-4000-8000-00000000000a", "bbbbbbbb-0000-4000-8000-00000000000b"
	base := File{Format: 1, ClusterID: "cccccccc-0000-4000-8000-00000000000c", Epoch: 1, Index: 3}
	for _, c := range []struct {
		name    string
		members []Member
		ok      bool
	}{
		{"two API pins", []Member{m(a, pin("1"), pin("3")), m(b, pin("2"), pin("4"))}, true},
		{"none", []Member{m(a, pin("1"), ""), m(b, pin("2"), "")}, true},
		{"twice", []Member{m(a, pin("1"), pin("3")), m(b, pin("2"), pin("3"))}, false},
		{"another's node pin", []Member{m(a, pin("1"), pin("2")), m(b, pin("2"), "")}, false},
		{"a later member's node pin", []Member{m(a, pin("1"), ""), m(b, pin("2"), pin("1"))}, false},
		{"not a pin", []Member{m(a, pin("1"), "sha256:x")}, false},
	} {
		f := base
		f.Members = c.members
		if err := f.validate(); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
