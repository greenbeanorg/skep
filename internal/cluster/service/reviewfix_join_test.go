package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/ids"
)

// pad writes 2*n entries (an admission and its withdrawal, n times), so
// that a host catching up from an empty log gets the history in more than
// one AppendEntries batch (the library's MaxAppendEntries is 64), as the
// lab's log of about 150 entries did.
func (h *harness) pad(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		id, err := ids.NewUUID()
		if err != nil {
			h.t.Fatal(err)
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		name := fmt.Sprintf("pad%d", i)
		req, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: id, Name: name,
			Address: fmt.Sprintf("127.89.250.%d:8444", i+1), Pin: "sha256:" + hex.EncodeToString(b)}})
		if err != nil {
			h.t.Fatal(err)
		}
		l := h.leader()
		if _, err := l.svc.Admit(context.Background(), req, "root"); err != nil {
			h.t.Fatalf("pad admit: %v", err)
		}
		if _, err := l.svc.Withdraw(context.Background(), name, "root"); err != nil {
			h.t.Fatalf("pad withdraw: %v", err)
		}
	}
}

// copyTree copies a data directory with its modes (the key's 0600 kept).
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(dst, info.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// replaceTree makes dir hold exactly what src holds.
func replaceTree(t *testing.T, src, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	copyTree(t, src, dir)
}

// F1 of the 8b-2b review: a host removed from a cluster while it joins runs
// "join --abandon", which keeps its key; another member rotates its key;
// the host joins the same cluster again with the same key. The log's
// history holds its older member entry with its current pin (state joining
// in the first case; state member in the second, where the cluster
// promoted it while it was stopped and removed it before it applied that).
// It must keep accepting its answer's nodes until it applies this join's
// entry, become a voter, delete join.json only at this join's entry, and
// never enter removed mode on the way.
func TestRejoinAfterAbandonWithTheSameKey(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		name := "removed while joining"
		if promoted {
			name = "removed after its promotion"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			defer h.stopAll()
			h.cluster("smith", "langrishe", "star")
			j := h.node("wu")
			h.start(j)
			h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
			pin := j.key.Pin()
			if out := h.join(j, h.admit(h.leader(), j)); out.Outcome != JoinAccepted {
				t.Fatalf("join: %+v", out)
			}
			h.eventually("wu is a non-voter", 30*time.Second, func() bool {
				servers, _ := h.leader().st.Servers()
				_, ok := findServer(servers, j.id)
				return ok
			})
			if promoted {
				// The host as it was before its promotion, kept to be
				// put back: as if it had stopped then and stayed down
				// through its promotion and removal.
				h.stop(j)
				before := t.TempDir()
				copyTree(t, j.dir, before)
				h.start(j)
				h.joined(j, 60*time.Second)
				h.pad(40)
				h.remove(h.leader(), j.name)
				h.gone(j, 60*time.Second)
				h.eventually("wu knows it was removed", 30*time.Second, func() bool { return removedMode(j) })
				h.stop(j)
				replaceTree(t, before, j.dir)
			} else {
				h.pad(40)
				h.remove(h.leader(), j.name)
				h.gone(j, 60*time.Second)
				h.eventually("wu knows it was removed", 30*time.Second, func() bool { return removedMode(j) })
				h.stop(j)
			}
			if out := StageAbandon(j.dir, j.cfg, j.key, 3*time.Second); out.Outcome != "restored" {
				t.Fatalf("abandon: %+v", out)
			}
			h.start(j)
			h.eventually("wu leads its own cluster again", 20*time.Second, func() bool { return j.st != nil && j.st.IsLeader() })
			// Another member, the leader, rotates its key (with the old
			// rule, the history's membership refuses the leader's new
			// key and the host stops catching up, lab case 9.7b).
			x := h.leader()
			res, err := x.svc.Rotate(t.Context(), "root")
			if err != nil {
				t.Fatal(err)
			}
			// (wu runs its own one-node cluster, so not h.rotated.)
			h.eventually(x.name+" rotated on the cluster's members", 60*time.Second, func() bool {
				for _, n := range h.running() {
					if p, next := pinOn(n, x); n != j && (p != res.NewPin || next != "") {
						return false
					}
				}
				return x.svc.rotationState() == nil
			})

			if out := h.join(j, h.admit(h.leader(), j)); out.Outcome != JoinAccepted {
				t.Fatalf("join again: %+v", out)
			}
			if j.key.Pin() != pin {
				t.Fatal("the key changed")
			}
			js, err := joinstate.Read(j.dir)
			if err != nil || js == nil {
				t.Fatalf("join.json: %v", err)
			}
			op := joinOpID(j.id, js.OpID)
			h.eventually("wu is a voter in state member again", 90*time.Second, func() bool {
				if removedMode(j) {
					t.Fatal("wu entered removed mode")
				}
				if j.svc != nil && j.svc.acceptedNow().source == "state machine" {
					if m, ok := j.svc.machine.Member(j.id); !ok || m.JoinOp != op {
						t.Fatalf("wu accepts the state machine's members at its older entry %+v", m)
					}
				}
				if gone, _ := joinstate.Read(j.dir); gone == nil {
					if m, ok := j.svc.machine.Member(j.id); !ok || m.JoinOp != op || m.State != clusterstate.StateMember {
						t.Fatalf("join.json deleted at an entry that is not this join's: %+v", m)
					}
				}
				for _, n := range h.running() {
					if !voters(n)[j.id] || stateOf(n, j.id) != clusterstate.StateMember {
						return false
					}
				}
				return j.svc.joinState() == nil
			})
		})
	}
}

// F1, deterministic: the lab's race (a batch boundary inside the host's
// older membership) cannot be forced in-process, where the leader sends
// the whole log before the joiner applies it. A joining host whose state
// machine stands at an older join's promoted entry, with its current key,
// does not accept the state machine's members and does not end its join;
// at this join's promoted entry it does both.
func TestJoinStepEndsOnlyAtThisJoinsEntry(t *testing.T) {
	self, other := "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000b2"
	cluster := "00000000-0000-4000-8000-0000000000cc"
	key, err := nodekey.Create(t.TempDir(), self, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	machineAt := func(callOp string) *clusterstate.Machine {
		m := clusterstate.New()
		i := uint64(0)
		apply := func(typ, op string, d any) {
			i++
			e, err := clusterstate.NewEntry(typ, op, clusterstate.ActorSystem, other, d)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := e.Encode()
			if r := m.Apply(i, raw); r.Err != nil {
				t.Fatal(r.Err)
			}
		}
		apply(clusterstate.TypeClusterInit, "init", clusterstate.ClusterInit{ClusterID: cluster,
			Member: clusterstate.Member{ID: other, Name: "smith", Address: "127.0.0.2:8444", Pin: "sha256:" + strings.Repeat("d", 64), State: clusterstate.StateMember}})
		apply(clusterstate.TypePendingCreate, "p", clusterstate.PendingCreate{NodeID: self, Name: "star", Address: "127.0.0.1:8444", Pin: key.Pin(),
			ExpiresAt: time.Now().Add(time.Hour).Unix()})
		apply(clusterstate.TypeMemberJoin, joinOpID(self, callOp), clusterstate.MemberJoin{NodeID: self, Pin: key.Pin(), At: time.Now().Unix()})
		apply(clusterstate.TypeMemberPromoted, "promoted", clusterstate.MemberPromoted{NodeID: self})
		return m
	}
	for _, tc := range []struct {
		callOp string
		ends   bool
	}{{"an-older-join", false}, {"this-join", true}} {
		dir := t.TempDir()
		js := joinstate.State{Phase: joinstate.PhaseAccepted, OpID: "this-join", Standalone: layout.StandalonePrefix + "20261125T000000Z",
			Admission: joincode.Admission{ClusterID: cluster}}
		if err := os.Mkdir(filepath.Join(dir, js.Standalone), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := joinstate.Write(dir, js); err != nil {
			t.Fatal(err)
		}
		s := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "star"}, Key: key, DataDir: dir, Machine: machineAt(tc.callOp),
			Join: &js, Logger: discardLog()})
		if m, _ := s.machine.Member(self); m.State != clusterstate.StateMember || m.Pin != key.Pin() {
			t.Fatalf("setup: %+v", m)
		}
		if got := s.acceptedNow().source == "state machine"; got != tc.ends {
			t.Fatalf("%s: accepts the state machine's members: %v", tc.callOp, got)
		}
		s.joinStep(context.Background())
		left, _ := joinstate.Read(dir)
		if (left == nil) != tc.ends || fileExists(dir, js.Standalone) == tc.ends {
			t.Fatalf("%s: join.json left %v, standalone left %v", tc.callOp, left != nil, fileExists(dir, js.Standalone))
		}
	}
}
