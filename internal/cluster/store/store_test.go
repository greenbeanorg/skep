package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

const (
	nodeA       = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"
	testCluster = "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"
	pinA        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func openLeader(t *testing.T, dir string) *Node {
	t.Helper()
	n, err := Open(Options{Dir: dir, NodeID: nodeA})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitLeader(ctx); err != nil {
		n.Close()
		t.Fatal(err)
	}
	return n
}

func newCluster(t *testing.T, peerAddress string) (string, *Node) {
	t.Helper()
	dir := t.TempDir()
	if err := Bootstrap(Options{Dir: dir, NodeID: nodeA}, peerAddress); err != nil {
		t.Fatal(err)
	}
	n := openLeader(t, dir)
	e, err := clusterstate.NewEntry(clusterstate.TypeClusterInit, "op-init", clusterstate.ActorSystem, nodeA,
		clusterstate.ClusterInit{ClusterID: testCluster, Member: clusterstate.Member{
			ID: nodeA, Name: "smith", Address: peerAddress, Pin: pinA, State: clusterstate.StateMember}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Apply(e); err != nil {
		n.Close()
		t.Fatal(err)
	}
	return dir, n
}

func TestOpenWithoutStore(t *testing.T) {
	if _, err := Open(Options{Dir: t.TempDir(), NodeID: nodeA}); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("Open on an empty directory: %v", err)
	}
}

func TestBootstrapOneNodeAndRestart(t *testing.T) {
	dir, n := newCluster(t, "")
	servers, err := n.Servers()
	if err != nil {
		t.Fatal(err)
	}
	if !Alone(servers, nodeA) || servers[0].Address != NoAddress || !servers[0].Voter {
		t.Fatalf("servers %+v", servers)
	}
	if st := n.Stats(); st.State != "leader" || st.LeaderID != nodeA {
		t.Fatalf("stats %+v", st)
	}
	// A second Bootstrap never replaces a store.
	if err := Bootstrap(Options{Dir: dir, NodeID: nodeA}, ""); err == nil {
		t.Fatal("Bootstrap replaced an existing store")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// After a restart the node leads again and has the same state.
	n = openLeader(t, dir)
	defer n.Close()
	if err := n.Barrier(); err != nil {
		t.Fatal(err)
	}
	v := n.Machine().View()
	if v.ClusterID != testCluster || len(v.Members) != 1 || v.Members[0].Pin != pinA || v.Members[0].ID != nodeA {
		t.Fatalf("state after a restart: %+v", v)
	}
	// The store file is private to its owner.
	info, err := os.Stat(filepath.Join(dir, DBFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("raft.db: %v, mode %v", err, info.Mode().Perm())
	}
}

func TestSecondOpenIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the store's lock timeout")
	}
	dir, n := newCluster(t, "")
	defer n.Close()
	if _, err := Open(Options{Dir: dir, NodeID: nodeA}); err == nil {
		t.Fatal("the store was opened twice")
	}
}

// Every Skep entry in a fresh log carries a type and a version (ADR 0013
// B5): the log is read back entry by entry.
func TestEveryLogEntryIsLabelled(t *testing.T) {
	_, n := newCluster(t, "")
	defer n.Close()
	if _, err := n.SetOwnAddress("192.0.2.1:8444", "op-addr"); err != nil {
		t.Fatal(err)
	}
	first, _ := n.bolt.FirstIndex()
	last, _ := n.bolt.LastIndex()
	commands := 0
	for i := first; i <= last; i++ {
		var l raft.Log
		if err := n.bolt.GetLog(i, &l); err != nil {
			t.Fatal(err)
		}
		if l.Type != raft.LogCommand {
			continue
		}
		commands++
		e, err := clusterstate.Decode(l.Data)
		if err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
		if max, ok := clusterstate.Supported[e.Type]; !ok || e.Version < 1 || e.Version > max {
			t.Fatalf("index %d: type %q version %d", i, e.Type, e.Version)
		}
	}
	if commands != 2 {
		t.Fatalf("%d Skep entries in the log, want 2", commands)
	}
}

func TestSetOwnAddress(t *testing.T) {
	dir, n := newCluster(t, "")
	changed, err := n.SetOwnAddress("192.0.2.1:8444", "op-1")
	if err != nil || !changed {
		t.Fatalf("SetOwnAddress: %v, %v", changed, err)
	}
	check := func(n *Node, want, wantRaft string) {
		t.Helper()
		if mem, _ := n.Machine().Member(nodeA); mem.Address != want {
			t.Fatalf("member address %q, want %q", mem.Address, want)
		}
		servers, _ := n.Servers()
		if !Alone(servers, nodeA) || servers[0].Address != wantRaft || !servers[0].Voter {
			t.Fatalf("servers %+v, want address %q", servers, wantRaft)
		}
	}
	check(n, "192.0.2.1:8444", "192.0.2.1:8444")
	// Repeating it changes nothing.
	if changed, err := n.SetOwnAddress("192.0.2.1:8444", "op-2"); err != nil || changed {
		t.Fatalf("repeat: %v, %v", changed, err)
	}
	// Both records survive a restart, and the node still leads alone.
	n.Close()
	n = openLeader(t, dir)
	defer n.Close()
	if err := n.Barrier(); err != nil {
		t.Fatal(err)
	}
	check(n, "192.0.2.1:8444", "192.0.2.1:8444")
	// Back to no listener.
	if _, err := n.SetOwnAddress("", "op-3"); err != nil {
		t.Fatal(err)
	}
	check(n, "", NoAddress)
}

func TestSetOwnAddressRefusedWithTwoServers(t *testing.T) {
	_, n := newCluster(t, "")
	defer n.Close()
	// A second server (a non-voter does not change the quorum of one).
	if err := n.raft.AddNonvoter("other", "192.0.2.9:8444", 0, 5*time.Second).Error(); err != nil {
		t.Fatal(err)
	}
	servers, _ := n.Servers()
	if Alone(servers, nodeA) {
		t.Fatal("Alone with two servers")
	}
	if _, err := n.SetOwnAddress("192.0.2.1:8444", "op-1"); err == nil {
		t.Fatal("the address was changed with two servers in the Raft configuration")
	}
	if mem, _ := n.Machine().Member(nodeA); mem.Address != "" {
		t.Fatal("the refused change wrote the member entry")
	}
}

func TestSnapshotAndRestart(t *testing.T) {
	dir, n := newCluster(t, "")
	if _, err := n.SetOwnAddress("192.0.2.1:8444", "op-1"); err != nil {
		t.Fatal(err)
	}
	if err := n.Snapshot(); err != nil {
		t.Fatal(err)
	}
	n.Close()
	n = openLeader(t, dir)
	defer n.Close()
	if mem, ok := n.Machine().Member(nodeA); !ok || mem.Address != "192.0.2.1:8444" || mem.Pin != pinA {
		t.Fatalf("state restored from the snapshot: %+v", mem)
	}
}

func TestRootOwned(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "b"), nil, 0o600)
	got, err := RootOwned(dir, func(info fs.FileInfo) (uint32, bool) {
		if info.Name() == "b" {
			return 0, true
		}
		return 1000, true
	})
	if err != nil || len(got) != 1 || filepath.Base(got[0]) != "b" {
		t.Fatalf("RootOwned = %v, %v", got, err)
	}
}

// A store whose machine stalled at an entry this build cannot apply keeps
// that entry: no snapshot is taken past it, and a build that supports the
// type applies it and everything after it when the store is opened again.
func TestStalledEntryIsAppliedAfterAnUpgrade(t *testing.T) {
	const future = "test.future"
	dir, n := newCluster(t, "")
	raw := []byte(`{"type":"` + future + `","version":1,"op_id":"op-future","actor":"system","origin":"` + nodeA + `","data":{}}`)
	if err := n.raft.Apply(raw, applyTimeout).Error(); err != nil {
		n.Close()
		t.Fatal(err)
	}
	e, err := clusterstate.NewEntry(clusterstate.TypeMemberAddress, "op-after", clusterstate.ActorSystem, nodeA,
		clusterstate.MemberAddress{NodeID: nodeA, Address: "192.0.2.7:8444"})
	if err != nil {
		n.Close()
		t.Fatal(err)
	}
	if _, err := n.Apply(e); !errors.Is(err, clusterstate.ErrStalled) {
		n.Close()
		t.Fatalf("an entry after the stall: %v", err)
	}
	v := n.Machine().View()
	if v.Stalled == nil {
		n.Close()
		t.Fatal("the machine did not stall")
	}
	stalledAt := v.Stalled.Index
	if err := n.Snapshot(); err == nil {
		n.Close()
		t.Fatal("a snapshot was taken while the machine is stalled")
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	clusterstate.Supported[future] = 1
	t.Cleanup(func() { delete(clusterstate.Supported, future) })
	n = openLeader(t, dir)
	defer n.Close()
	if err := n.Barrier(); err != nil {
		t.Fatal(err)
	}
	v = n.Machine().View()
	if v.Stalled != nil || v.AppliedIndex <= stalledAt {
		t.Fatalf("after the upgrade: %+v (stalled at %d)", v, stalledAt)
	}
	if mem, _ := n.Machine().Member(nodeA); mem.Address != "192.0.2.7:8444" {
		t.Fatalf("the entry after the stalled one was not applied: %+v", mem)
	}
	if err := n.Snapshot(); err != nil {
		t.Fatalf("snapshot after the upgrade: %v", err)
	}
}
