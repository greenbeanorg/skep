package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// D2: the node cannot say while its state machine is stopped, while it is
// behind the applied-membership file, and while its join was refused.
func TestCannotSayRule(t *testing.T) {
	self := "00000000-0000-4000-8000-000000000001"
	dir := t.TempDir()
	w, err := membership.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := clusterstate.New()
	s := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "self"}, Machine: m, Writer: w, Logger: discardLog()})
	if s.cannotSay() {
		t.Fatal("an empty node without a file can say")
	}
	// Behind the file.
	if err := w.Write(clusterstate.Membership{ClusterID: "00000000-0000-4000-8000-0000000000cc", Epoch: 1, Index: 9,
		Members: []clusterstate.Member{{ID: self, Name: "self", Pin: "sha256:" + strings.Repeat("a", 64), State: clusterstate.StateMember}}}); err != nil {
		t.Fatal(err)
	}
	if !s.cannotSay() {
		t.Fatal("behind the applied-membership file")
	}
	// Stopped.
	m2 := clusterstate.New()
	m2.Apply(1, []byte(`{"type":"future.type","version":1,"op_id":"x","actor":"a","origin":"o","data":{}}`))
	s2 := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "self"}, Machine: m2, Logger: discardLog()})
	if !s2.cannotSay() {
		t.Fatal("a stopped state machine")
	}
	// A refused join.
	s3 := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "self"}, Join: &joinstate.State{Phase: joinstate.PhaseRefused}, Logger: discardLog()})
	if !s3.cannotSay() {
		t.Fatal("a refused join")
	}
	s3.join.Phase = joinstate.PhaseAccepted
	if s3.cannotSay() {
		t.Fatal("an accepted join can say")
	}
}

// D2 over the network: a member whose state machine has not reached its
// applied-membership file answers cannot-say to a key it does not know;
// once it has, the silent close (refused).
func TestCannotSayOverTheNetwork(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	x := follower(h)
	stranger := h.node("stranger") // a key nobody pins
	if res, err := raftnet.ProbeKey(x.cfg.PeerAddress(), x.key.Pin(), stranger.key.Certificate, 3*time.Second); res != raftnet.ProbeRefused {
		t.Fatalf("an up-to-date member: %s %v", res, err)
	}
	// Make x's machine look behind: its writer holds a later index.
	cur := x.svc.writer.Current()
	if err := x.svc.writer.Write(clusterstate.Membership{ClusterID: cur.ClusterID, Epoch: cur.Epoch, Index: cur.Index + 1000,
		Members: toMembers(cur.Members)}); err != nil {
		t.Fatal(err)
	}
	if res, err := raftnet.ProbeKey(x.cfg.PeerAddress(), x.key.Pin(), stranger.key.Certificate, 3*time.Second); res != raftnet.ProbeCannotSay {
		t.Fatalf("a member behind its file: %s %v", res, err)
	}
	// A member key is still accepted by the file's members.
	leader := h.leader()
	if res, err := raftnet.ProbeKey(x.cfg.PeerAddress(), x.key.Pin(), leader.key.Certificate, 3*time.Second); res != raftnet.ProbeAccepted {
		t.Fatalf("a member key while behind: %s %v", res, err)
	}
}

func toMembers(in []membership.Member) []clusterstate.Member {
	var out []clusterstate.Member
	for _, m := range in {
		out = append(out, clusterstate.Member{ID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin, State: m.State})
	}
	return out
}

// D5: the pool's normal use stays well under the per-node limit. Three
// nodes run for a while (status rounds every 300 ms in the harness,
// heartbeats, a few writes); the highest number of connections one node
// held open on another's listener at once is measured and logged.
func TestPoolUseUnderPerNodeLimit(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	max := 0
	stop := time.Now().Add(6 * time.Second)
	writes := 0
	for time.Now().Before(stop) {
		for _, n := range h.running() {
			n.svc.mu.Lock()
			l := n.svc.listener
			n.svc.mu.Unlock()
			if l == nil {
				continue
			}
			for g, c := range l.OpenPerGroup() {
				if strings.HasPrefix(g, "node:") && c > max {
					max = c
				}
			}
		}
		if writes < 20 {
			e, _ := clusterstate.NewEntry(clusterstate.TypePendingWithdraw, "w-"+time.Now().Format("150405.000000"), "root", leader.id,
				clusterstate.PendingWithdraw{NodeID: leader.id, Name: "nobody"})
			_, _ = leader.st.Apply(e) // refused at apply; it still goes through the log
			writes++
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("highest number of open connections of one node on another's listener: %d (limit %d)", max, peerlistener.MaxPerNode)
	if max == 0 || max > peerlistener.MaxPerNode/2 {
		t.Fatalf("highest per-node use %d is not well under the limit %d", max, peerlistener.MaxPerNode)
	}
}

// D6: a member removed while it was down finds, when it returns, that
// every member that answers refuses its key: after three status rounds its
// health warns, naming leave --wipe, and it does not enter removed mode.
func TestKeyRefusedWarning(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	h.stop(x)
	h.remove(leader, x.name)
	h.eventually("x is gone on the leader", 60*time.Second, func() bool { _, ok := leader.svc.machine.Member(x.id); return !ok })
	h.start(x)
	h.eventually("key_refused on x", 30*time.Second, func() bool {
		c := x.svc.Health(context.Background()).Checks["key_refused"]
		return strings.Contains(c.Error, "leave --wipe") && strings.Contains(c.Error, "probably removed while it was down")
	})
	hl := x.svc.Health(context.Background())
	if c := hl.Checks["key_refused"]; c.Critical {
		t.Fatalf("key_refused must be a warning: %+v", c)
	}
	if x.svc.isRemoved() {
		t.Fatal("the check put the node in removed mode")
	}
	// The members are not warned about their own keys.
	for _, n := range h.running() {
		if n == x {
			continue
		}
		if c := n.svc.Health(context.Background()).Checks["key_refused"]; c.Error != "" {
			t.Fatalf("%s: %s", n.name, c.Error)
		}
	}
}
