package service

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// stopAll stops every node before the test's temporary directories are
// removed (they are cleaned up before the harness's own clean-up runs).
func (h *harness) stopAll() {
	for _, n := range h.nodes {
		h.stop(n)
	}
}

// peerOf is n as a raftnet.Peer (the sender of a notice).
func peerOf(n *tnode) raftnet.Peer {
	return raftnet.Peer{NodeID: n.id, Name: n.name, Address: n.cfg.PeerAddress(), Pin: n.key.Pin(), Kind: raftnet.KindMember}
}

// sendNotice sends the removed notice from `from` to x over the network,
// as notifyRemoved does.
func sendNotice(t *testing.T, from, x *tnode, clusterID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := from.svc.caller.Call(ctx, x.cfg.PeerAddress(), x.key.Pin(), raftnet.OpRemoved, RemovedNotice{ClusterID: clusterID, NodeID: x.id}, nil); err != nil {
		t.Fatalf("notice from %s to %s: %v", from.name, x.name, err)
	}
}

// F1: a valid notice from one member is not believed while the others
// accept the key: the check runs three rounds and drops it. A second
// notice during a check is dropped. After a real removal, a notice is
// confirmed by the members refusing the key.
func TestRemovedNoticeIsChecked(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	cid := leader.svc.machine.View().ClusterID
	start := time.Now()
	sendNotice(t, leader, x, cid)
	sendNotice(t, leader, x, cid)
	h.eventually("the notice is dropped", 30*time.Second, func() bool {
		return strings.Contains(x.logs.String(), "a removed notice was dropped")
	})
	if removedMode(x) {
		t.Fatal("a notice was believed while the members accept the key")
	}
	if d := time.Since(start); d < 2*noticePause {
		t.Fatalf("the notice was dropped after %s, before three rounds", d)
	}
	if !strings.Contains(x.logs.String(), "while another is being checked") {
		t.Fatal("a second notice during a check was not dropped")
	}
	if c := strings.Count(x.logs.String(), "a removed notice is not confirmed yet"); c != noticeRounds {
		t.Fatalf("%d rounds, want %d", c, noticeRounds)
	}
	// A real removal. x learns it by itself (from its own log or the
	// leader's notice); a notice checked by a service that has x's
	// identity and the cluster's view is confirmed by the two members
	// refusing x's key. (A node stopped during its own removal restarts
	// without the cluster's state applied and refuses the notice as not
	// for its cluster: backlog, step 8b-2b.)
	h.remove(leader, x.name)
	h.gone(x, 60*time.Second)
	h.eventually(x.name+" knows it was removed", 30*time.Second, func() bool { return removedMode(x) })
	l := h.leader()
	s := New(Options{Config: x.cfg, Key: x.key, DataDir: t.TempDir(), Machine: l.svc.machine, Logger: discardLog()})
	if _, err := s.handleRemovedNotice(peerOf(l), RemovedNotice{ClusterID: cid, NodeID: x.id}); err != nil {
		t.Fatal(err)
	}
	h.eventually("the confirmed notice is believed", 30*time.Second, func() bool { return s.isRemoved() })
	if how := s.removedRecord().How; how != "notice from "+l.name+", confirmed by 2 members refusing this key" {
		t.Fatalf("how: %q", how)
	}
}

// F1 on a joining node: the nodes it probes are its admission's (Peers).
// While the admission's pending key is accepted, a notice is dropped; once
// it is withdrawn and every member refuses the key, a notice is believed.
func TestRemovedNoticeOnAJoiningNode(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	leader := h.leader()
	j := h.node("wu")
	adm := h.admit(leader, j)
	s := New(Options{Config: j.cfg, Key: j.key, DataDir: t.TempDir(), Logger: discardLog(),
		Join: &joinstate.State{Phase: joinstate.PhaseUnknown, Admission: adm, Standalone: "standalone.20260101T000000Z"}})
	notice := RemovedNotice{ClusterID: adm.ClusterID, NodeID: j.id}
	if _, err := s.handleRemovedNotice(peerOf(leader), notice); err != nil {
		t.Fatal(err)
	}
	h.eventually("the check ends", 30*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.noticeBusy
	})
	if s.isRemoved() {
		t.Fatal("believed while the members accept the pending key")
	}
	if _, err := leader.svc.Withdraw(context.Background(), j.name, "root"); err != nil {
		t.Fatal(err)
	}
	for _, n := range h.running() {
		n := n
		h.eventually(n.name+" refuses wu's key", 10*time.Second, func() bool { return refusedIn(n, j) })
	}
	if _, err := s.handleRemovedNotice(peerOf(leader), notice); err != nil {
		t.Fatal(err)
	}
	h.eventually("wu believes the notice", 30*time.Second, func() bool { return s.isRemoved() })
	if !strings.Contains(s.removedRecord().How, "confirmed by 2 members refusing this key") {
		t.Fatalf("how: %q", s.removedRecord().How)
	}
	// F7: a removed node that is joining is told to abandon, not to wipe.
	hl := s.Health(context.Background())
	if c := hl.Checks["removed"].Error; !strings.Contains(c, "join --abandon") || strings.Contains(c, "leave --wipe") {
		t.Fatalf("health: %q", c)
	}
	if st := s.Status(context.Background()); st.Join == nil || st.Removed == nil {
		t.Fatalf("status: %+v", st)
	}
}

// F4: while a member is being removed, the leader refuses an admission
// before proposing it (409 refused, naming the member), and the state
// machine refuses a pending.create at apply.
func TestNoAdmissionDuringARemoval(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("a", "b", "c", "d")
	leader := h.leader()
	x := follower(h)
	frozen := make(chan struct{}, 1)
	leader.svc.mu.Lock()
	leader.svc.afterRemovalStep = func(n int, id string) bool {
		if n != 0 || id != x.id {
			return false
		}
		frozen <- struct{}{}
		return true
	}
	leader.svc.mu.Unlock()
	h.remove(leader, x.name)
	select {
	case <-frozen:
	case <-time.After(60 * time.Second):
		t.Fatal("step 0 was not reached")
	}
	line, _, gn := ghost(t, "ghost", "127.0.88.9")
	_, err := leader.svc.Admit(context.Background(), line, "root")
	if opCode(err) != "refused" || !strings.Contains(err.Error(), x.name+" is being removed") {
		t.Fatalf("admit during a removal: %v", err)
	}
	e, _ := clusterstate.NewEntry(clusterstate.TypePendingCreate, "p-1", "root", leader.id, clusterstate.PendingCreate{
		NodeID: gn.NodeID, Name: gn.Name, Address: gn.Address, Pin: gn.Pin, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	_, err = leader.st.Apply(e)
	if opCode(applyError(err)) != "refused" || !strings.Contains(err.Error(), "member "+x.name+" is being removed") {
		t.Fatalf("pending.create at apply during a removal: %v", err)
	}
	if len(pendings(leader)) != 0 {
		t.Fatal("a pending entry was written")
	}
}

// F5: a member that applied the removal and then stops answering is not
// named; one this node never heard from is, and so is one below the
// removal's index, each saying which it is.
func TestRemovalAppliedIsRemembered(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("a", "b", "c", "d")
	leader := h.leader()
	x := follower(h)
	h.remove(leader, x.name)
	h.gone(x, 60*time.Second)
	h.eventually("nobody named", 30*time.Second, func() bool {
		return leader.svc.Health(context.Background()).Checks["removal_applied"].Error == ""
	})
	y := follower(h, x)
	h.stop(y)
	// Several status rounds (300 ms apart in the harness) with y down.
	h.eventually("a status round without an answer from "+y.name, 20*time.Second, func() bool {
		leader.svc.mu.Lock()
		defer leader.svc.mu.Unlock()
		r, ok := leader.svc.answers[y.id]
		return ok && r.err != nil
	})
	time.Sleep(2 * time.Second)
	if c := leader.svc.Health(context.Background()).Checks["removal_applied"].Error; c != "" {
		t.Fatalf("a member that applied the removal is named once it is down: %q", c)
	}
	h.start(y)

	// Unit part: what is named and how.
	s := New(Options{Config: clusterconf.Config{NodeID: newID(t), NodeName: "self"}})
	a, b, c := newID(t), newID(t), newID(t)
	s.highest[a], s.highest[c] = 12, 5
	view := clusterstate.View{RemovedIndex: 10, Members: []clusterstate.Member{
		{ID: a, Name: "a", State: clusterstate.StateMember},
		{ID: b, Name: "b", State: clusterstate.StateMember},
		{ID: c, Name: "c", State: clusterstate.StateMember},
	}}
	s.answers[a] = statusResult{err: context.DeadlineExceeded}
	if got := s.unappliedRemoval(view); got != nil {
		t.Fatalf("named before the first status round has ended: %v", got)
	}
	s.firstRound = true // the first status round since the start has ended
	got := strings.Join(s.unappliedRemoval(view), "; ")
	if got != "b (no answer since this node started); c (applied 5, the removal is 10)" {
		t.Fatalf("named: %q", got)
	}
}

// F6: the status round runs in its own goroutine, one at a time: with a
// member that holds the connection without answering, statusStep returns
// at once and the leader loop keeps its pace meanwhile.
func TestStatusRoundDoesNotBlockTheStepLoop(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	y := follower(h)
	h.stop(y)
	ln, err := net.Listen("tcp", y.cfg.PeerAddress()) // accepts and never answers
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		var held []net.Conn
		for {
			c, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, c)
		}
	}()
	busy := func() bool {
		leader.svc.mu.Lock()
		defer leader.svc.mu.Unlock()
		return leader.svc.statusBusy
	}
	h.eventually("a status round waits for "+y.name, 20*time.Second, busy)
	nextLeader := func() time.Time {
		leader.svc.mu.Lock()
		defer leader.svc.mu.Unlock()
		return leader.svc.nextLeader
	}
	t0 := time.Now()
	first, advanced := nextLeader(), 0
	for time.Since(t0) < 2*time.Second && busy() {
		time.Sleep(100 * time.Millisecond)
		if n := nextLeader(); n.After(first) {
			advanced++
			first = n
		}
	}
	if advanced < 3 {
		t.Fatalf("the leader loop ran %d times in %s while the status round waited", advanced, time.Since(t0).Round(time.Millisecond))
	}
	// One at a time: a step while the round runs does not start another.
	if busy() {
		begin := time.Now()
		leader.svc.mu.Lock()
		leader.svc.nextStatus = time.Time{}
		leader.svc.mu.Unlock()
		leader.svc.statusStep(context.Background())
		if d := time.Since(begin); d > 200*time.Millisecond {
			t.Fatalf("statusStep took %s", d)
		}
	}
}

// F3: join --abandon refuses, changing nothing and asking nobody, when
// join.json names no moved-aside state that exists; CheckAbandon writes
// nothing either.
func TestAbandonWithNothingToPutBack(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	n := h.node("smith")
	_, _, gn := ghost(t, "ghost", "127.0.88.9")
	for _, standalone := range []string{"standalone.20260101T000000Z", "elsewhere"} {
		js := joinstate.State{Phase: joinstate.PhaseRefused, OpID: newID(t), Standalone: standalone,
			Admission: joincode.Admission{ClusterID: newID(t), Members: []joincode.Node{gn}, ExpiresAt: 1,
				For: joincode.Node{NodeID: n.id, Name: n.name, Address: n.cfg.PeerAddress(), Pin: n.key.Pin()}}}
		if err := joinstate.Write(n.dir, js); err != nil {
			t.Fatal(err)
		}
		for _, f := range []func() AbandonOutcome{
			func() AbandonOutcome { return StageAbandon(n.dir, n.cfg, n.key, time.Second) },
			func() AbandonOutcome { return CheckAbandon(n.dir, n.cfg, n.key, time.Second) },
		} {
			out := f()
			if out.Outcome != "no_standalone" || len(out.Probes) != 0 || !strings.Contains(out.Message, "leave --wipe") {
				t.Fatalf("%s: %+v", standalone, out)
			}
			if got, _ := joinstate.Read(n.dir); got == nil || got.Phase != joinstate.PhaseRefused {
				t.Fatal("join.json changed")
			}
			if ok, _ := layout.Exists(layout.State(n.dir)); !ok {
				t.Fatal("state/ is gone")
			}
		}
	}
}

// removeFails is a store whose RemoveServer returns err.
type removeFails struct {
	Store
	err error
}

func (r removeFails) RemoveServer(string) error { return r.err }

// F9: the ghost sweep says "was removed" only when RemoveServer succeeded.
func TestGhostSweepLogsAFailure(t *testing.T) {
	for _, err := range []error{errors.New("injected: not the leader"), nil} {
		logs := &logBuffer{}
		s := New(Options{Config: clusterconf.Config{NodeID: newID(t)}, Logger: slog.New(slog.NewTextHandler(logs, nil))})
		s.store = removeFails{err: err}
		if !s.ghostSweep(clusterstate.View{}, []store.Server{{ID: newID(t), Address: "127.0.88.9:8444"}}) {
			t.Fatal("no sweep")
		}
		failed := strings.Contains(logs.String(), "failed; it will be tried again") && strings.Contains(logs.String(), "injected")
		removed := strings.Contains(logs.String(), "was removed")
		if (err != nil) != failed || (err != nil) == removed {
			t.Fatalf("error %v: log %q", err, logs.String())
		}
	}
}
