package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// running returns the nodes that run a store (not stopped, not removed).
func (h *harness) running() []*tnode {
	var out []*tnode
	for _, n := range h.nodes {
		if n.svc != nil && n.st != nil && !n.svc.isRemoved() {
			out = append(out, n)
		}
	}
	return out
}

// gone waits until x has no member entry and is no Raft server on every
// running node but x.
func (h *harness) gone(x *tnode, limit time.Duration) {
	h.t.Helper()
	h.eventually(x.name+" removed on every running member", limit, func() bool {
		for _, n := range h.running() {
			if n == x {
				continue
			}
			if _, ok := n.svc.machine.Member(x.id); ok {
				return false
			}
			servers, err := n.st.Servers()
			if err != nil {
				return false
			}
			if _, in := findServer(servers, x.id); in {
				return false
			}
		}
		return true
	})
}

func (h *harness) remove(on *tnode, name string) localapi.RemoveResult {
	h.t.Helper()
	res, err := on.svc.Remove(context.Background(), localapi.RemoveRequest{Name: name}, "root")
	if err != nil {
		h.t.Fatalf("remove %s on %s: %v", name, on.name, err)
	}
	return res
}

func removedMode(n *tnode) bool { return n.svc != nil && n.svc.isRemoved() }

func follower(h *harness, not ...*tnode) *tnode {
	for _, n := range h.running() {
		if n.st.IsLeader() {
			continue
		}
		skip := false
		for _, x := range not {
			skip = skip || n == x
		}
		if !skip {
			return n
		}
	}
	return nil
}

// refusedIn says whether x's key is refused in the handshake by n.
func refusedIn(n, x *tnode) bool {
	c, err := raftnet.DialPinned(n.cfg.PeerAddress(), n.key.Pin(), x.key.Certificate, 3*time.Second)
	if err == nil {
		c.Close()
		return false
	}
	return true
}

// Removing a follower of three: two voters remain, the removed key is
// refused in the handshake by both, the removed node says so in status and
// health, also after a restart, and nothing names a member that has not
// applied the removal (lab case 4.2, in-process).
func TestRemoveAFollower(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	other := follower(h, x)
	res := h.remove(other, x.name) // passed to the leader
	if res.NodeID != x.id || res.HandedOver {
		t.Fatalf("result %+v", res)
	}
	h.gone(x, 60*time.Second)
	for _, n := range []*tnode{leader, other} {
		if v := voters(n); len(v) != 2 || v[x.id] {
			t.Fatalf("%s voters %v", n.name, v)
		}
		n := n
		h.eventually(x.name+"'s key refused in the handshake by "+n.name, 10*time.Second, func() bool { return refusedIn(n, x) })
		h.eventually("nothing on "+n.name+" names a member that has not applied the removal", 20*time.Second, func() bool {
			hl := n.svc.Health(context.Background())
			return hl.Checks["removal_applied"].Error == "" && hl.Checks["removal"].Error == "" && hl.Status == "ok"
		})
		if !strings.Contains(n.logs.String(), "cluster membership: member removed") || !strings.Contains(n.logs.String(), "cluster membership: member removing") {
			t.Fatalf("%s did not log the removal", n.name)
		}
	}
	h.eventually(x.name+" knows it was removed", 30*time.Second, func() bool { return removedMode(x) })
	checkRemovedNode(t, x)
	// After a restart of its service: the same, and no listener.
	h.stop(x)
	h.start(x)
	if !removedMode(x) {
		t.Fatal("the removed node forgot it after a restart")
	}
	checkRemovedNode(t, x)
	if c, err := raftnet.DialPinned(x.cfg.PeerAddress(), x.key.Pin(), leader.key.Certificate, time.Second); err == nil {
		c.Close()
		t.Fatal("the removed node's listener is open after a restart")
	}
}

func checkRemovedNode(t *testing.T, x *tnode) {
	t.Helper()
	hl := x.svc.Health(context.Background())
	c := hl.Checks["removed"]
	if hl.Status == "ok" || !strings.Contains(c.Error, "leave --wipe") || !strings.Contains(c.Error, x.svc.removedRecord().ClusterID) {
		t.Fatalf("health of the removed node: %+v", hl)
	}
	st := x.svc.Status(context.Background())
	if st.Removed == nil || st.AcceptedBy != "none (removed)" {
		t.Fatalf("status of the removed node: %+v", st)
	}
	if _, err := x.svc.Admit(context.Background(), "x", "root"); opCode(err) != "removed" {
		t.Fatalf("admit on a removed node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.state(), layout.RemovedFile)); err != nil {
		t.Fatal("no removal record")
	}
}

// The leader asked to remove itself hands leadership over first and the
// new leader removes it (D6; lab case 4.4, in-process).
func TestRemoveTheLeader(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	old := h.leader()
	res := h.remove(old, old.name)
	if !res.HandedOver || res.NodeID != old.id {
		t.Fatalf("result %+v", res)
	}
	h.gone(old, 60*time.Second)
	h.eventually(old.name+" knows it was removed", 30*time.Second, func() bool { return removedMode(old) })
	l := h.leader()
	if l == nil || l == old || len(voters(l)) != 2 {
		t.Fatalf("after the removal: leader %v", l)
	}
	if !strings.Contains(old.logs.String(), "leadership handed over before this node's removal") {
		t.Fatal("the old leader did not hand the leadership over")
	}
}

// The leader stopped after each of the three records: a new leader
// finishes the removal. Four nodes, so that every step keeps a quorum
// with the leader stopped (three of four before RemoveServer, two of
// three after it).
func TestNewLeaderFinishesARemoval(t *testing.T) {
	for step := 0; step <= 2; step++ {
		t.Run(fmt.Sprintf("after step %d", step), func(t *testing.T) {
			h := newHarness(t)
			h.cluster("a", "b", "c", "d")
			old := h.leader()
			x := follower(h)
			frozen := make(chan struct{}, 1)
			old.svc.mu.Lock()
			old.svc.afterRemovalStep = func(n int, id string) bool {
				if n != step || id != x.id {
					return false
				}
				frozen <- struct{}{}
				return true
			}
			old.svc.mu.Unlock()
			h.remove(old, x.name)
			select {
			case <-frozen:
			case <-time.After(60 * time.Second):
				t.Fatalf("step %d was not reached", step)
			}
			h.stop(old)
			h.eventually("a new leader", 30*time.Second, func() bool {
				l := h.leader()
				return l != nil && l != old
			})
			h.gone(x, 90*time.Second)
			// The old leader comes back as a follower and catches up.
			h.start(old)
			h.eventually(old.name+" applied the removal", 60*time.Second, func() bool {
				v := old.svc.machine.View()
				_, ok := old.svc.machine.Member(x.id)
				servers, _ := old.st.Servers()
				_, in := findServer(servers, x.id)
				return v.ClusterID != "" && len(v.Members) == 3 && !ok && !in
			})
			for _, n := range h.running() {
				if n == x {
					continue
				}
				if len(voters(n)) != 3 || voters(n)[x.id] {
					t.Fatalf("%s voters %v", n.name, voters(n))
				}
			}
		})
	}
}

// Removing a joining member clears a stuck join (D7a allows it), another
// member's removal is refused meanwhile, the joiner is told, and "join
// --abandon" then restores its one-node state (D9's success path, which
// three lab hosts cannot produce on purpose).
func TestRemoveAJoiningMemberAndAbandon(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe", "star")
	down := follower(h)
	h.stop(down)
	j := h.node("wu")
	h.start(j)
	h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
	ownCluster := j.svc.machine.View().ClusterID
	ownPin := j.key.Pin()
	adm := h.admit(h.leader(), j)
	if out := h.join(j, adm); out.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", out)
	}
	h.eventually("wu is a working non-voter", 30*time.Second, func() bool {
		servers, _ := h.leader().st.Servers()
		sv, ok := findServer(servers, j.id)
		return ok && !sv.Voter
	})
	leader := h.leader()
	other := follower(h, j)
	// D7a: another member cannot be removed while wu joins.
	if _, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: other.name}, "root"); opCode(err) != "refused" ||
		!strings.Contains(err.Error(), "joining") {
		t.Fatalf("removal during a join: %v", err)
	}
	// Before the removal, join --abandon refuses: members accept its key.
	h.stop(j)
	out := StageAbandon(j.dir, j.cfg, j.key, 3*time.Second)
	if out.Outcome != "accepted" || len(out.AcceptedBy) == 0 {
		t.Fatalf("abandon while accepted: %+v", out)
	}
	if js, _ := joinstate.Read(j.dir); js == nil || js.Phase == joinstate.PhaseAbandoning {
		t.Fatal("a refused abandon changed join.json")
	}
	h.start(j)
	// Remove the joining member itself.
	h.remove(leader, j.name)
	h.gone(j, 60*time.Second)
	h.eventually("wu knows it was removed", 30*time.Second, func() bool { return removedMode(j) })
	// join --abandon: the members answer and refuse its key now.
	h.stop(j)
	out = StageAbandon(j.dir, j.cfg, j.key, 3*time.Second)
	if out.Outcome != "restored" {
		t.Fatalf("abandon after the removal: %+v", out)
	}
	if js, _ := joinstate.Read(j.dir); js != nil {
		t.Fatal("join.json is left")
	}
	if len(standalones(t, j.dir)) != 0 {
		t.Fatal("a moved-aside state is left")
	}
	h.start(j)
	h.eventually("wu leads its own one-node cluster again", 20*time.Second, func() bool { return j.st != nil && j.st.IsLeader() })
	if v := j.svc.machine.View(); v.ClusterID != ownCluster || j.key.Pin() != ownPin || len(v.Members) != 1 {
		t.Fatalf("after abandon: %+v", v)
	}
	h.start(down)
	_ = nodes
}

// D7b: an admission issued before a removal is void.
func TestRemovalWithdrawsPendingAdmissions(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	j := h.node("wu")
	h.start(j)
	adm := h.admit(leader, j)
	x := follower(h)
	h.remove(leader, x.name)
	for _, n := range h.running() {
		n := n
		h.eventually("no pending entry on "+n.name, 10*time.Second, func() bool { return len(pendings(n)) == 0 })
	}
	if out := h.join(j, adm); out.Outcome == JoinAccepted {
		t.Fatalf("a join with an admission from before the removal: %+v", out)
	}
}

// D7c: without x, fewer than a majority of the remaining voters answer: the
// removal is refused and nothing is written; with -allow-unanswered and the
// cluster ID it starts, cannot finish while the member is down (the
// configuration without x needs it), health says what it waits for, and it
// finishes by itself when the member returns (lab case 4.5, in-process).
func TestRemovalWhileAMemberIsDown(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	down := follower(h, x)
	h.stop(down)
	_, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: x.name}, "root")
	if opCode(err) != "voters_unanswered" || !strings.Contains(err.Error(), down.name) {
		t.Fatalf("removal without the override: %v", err)
	}
	if stateOf(leader, x.id) != clusterstate.StateMember {
		t.Fatal("something was written")
	}
	// A wrong cluster ID is refused.
	if _, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: x.name, AllowUnanswered: true, ClusterID: newID(t)}, "root"); opCode(err) != "refused" {
		t.Fatalf("wrong cluster ID: %v", err)
	}
	cid := leader.svc.machine.View().ClusterID
	res, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: x.name, AllowUnanswered: true, ClusterID: cid}, "root")
	if err != nil || len(res.Unanswered) != 1 {
		t.Fatalf("removal with the override: %+v %v", res, err)
	}
	time.Sleep(5 * time.Second)
	if _, ok := leader.svc.machine.Member(x.id); !ok {
		t.Fatal("the removal finished while the member it needs is down")
	}
	h.eventually("health on "+leader.name+" says what the removal waits for", 30*time.Second, func() bool {
		c := leader.svc.Health(context.Background()).Checks["removal"]
		return strings.Contains(c.Error, "the removal of "+x.name+" waits for the Raft configuration without it to commit") &&
			strings.Contains(c.Error, down.name)
	})
	if _, _, committed, err := leader.st.Configuration(); err != nil || committed {
		t.Fatalf("the configuration without %s reads as committed while %s is down (%v)", x.name, down.name, err)
	}
	start := time.Now()
	h.start(down)
	h.gone(x, 90*time.Second)
	t.Logf("the removal finished %s after %s was started", time.Since(start).Round(100*time.Millisecond), down.name)
}

// D12: a node removed while one of four members is down (the removal can
// finish: three voters remain, two of them up). Health on every running
// member names the stopped member as not having applied the removal, and
// stops naming it once it has returned and caught up (ADR 0013 B2).
func TestHealthNamesMembersThatHaveNotAppliedARemoval(t *testing.T) {
	h := newHarness(t)
	h.cluster("a", "b", "c", "d")
	x := follower(h)
	down := follower(h, x)
	h.stop(down)
	leader := h.leader()
	h.remove(leader, x.name)
	h.gone(x, 60*time.Second)
	for _, n := range h.running() {
		if n == x {
			continue
		}
		n := n
		h.eventually(n.name+" names "+down.name+" and nobody else", 30*time.Second, func() bool {
			c := n.svc.Health(context.Background()).Checks["removal_applied"]
			st := n.svc.Status(context.Background())
			return strings.Contains(c.Error, down.name) && strings.Contains(c.Error, "wiped") &&
				len(st.UnappliedRemoval) == 1 && strings.HasPrefix(st.UnappliedRemoval[0], down.name+" ")
		})
	}
	h.start(down)
	for _, n := range h.running() {
		if n == x {
			continue
		}
		n := n
		h.eventually(n.name+" no longer names anyone", 60*time.Second, func() bool {
			return n.svc.Health(context.Background()).Checks["removal_applied"].Error == ""
		})
	}
}

// Ghost sweep (D5f): a server in the Raft configuration without a member
// entry is removed by the leader loop and logged.
func TestGhostSweep(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe")
	leader := h.leader()
	ghostID := newID(t)
	// Bypass the leader loop on purpose.
	leader.svc.mu.Lock()
	leader.svc.frozen = true
	leader.svc.mu.Unlock()
	if err := leader.st.AddNonvoter(ghostID, "127.0.88.9:8444"); err != nil {
		t.Fatal(err)
	}
	leader.svc.mu.Lock()
	leader.svc.frozen = false
	leader.svc.mu.Unlock()
	h.eventually("the ghost removed", 30*time.Second, func() bool {
		servers, _ := leader.st.Servers()
		_, in := findServer(servers, ghostID)
		return !in
	})
	if !strings.Contains(leader.logs.String(), "ghost removed") {
		t.Fatal("the sweep was not logged")
	}
}

// termStore is a one-node store whose term the test sets: the leadership
// "lost and regained" between two rounds without IsLeader ever being seen
// false.
type termStore struct {
	*barrierStore
	term uint64
}

func (s *termStore) Term() uint64 { return s.term }

// D5a: the barrier is repeated whenever the term differs from the term of
// the last successful barrier, not only when a was-leader flag flips.
func TestBarrierFollowsTheTerm(t *testing.T) {
	h := newHarness(t)
	tn := h.node("smith")
	n, err := store.Open(store.Options{Dir: tn.state(), NodeID: tn.id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	wctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitLeader(wctx); err != nil {
		t.Fatal(err)
	}
	ts := &termStore{barrierStore: &barrierStore{Node: n}, term: 5}
	c := &clock{}
	s := New(Options{Config: tn.cfg, Key: tn.key, Now: c.now, LeaderInterval: time.Hour})
	s.SetStore(ts, nil)
	ctx := context.Background()
	count := func() int {
		k := 0
		for _, x := range ts.log() {
			if x == "barrier" {
				k++
			}
		}
		return k
	}
	s.leaderStep(ctx)
	if count() != 1 {
		t.Fatalf("first round: %v", ts.log())
	}
	s.leaderStep(ctx)
	if count() != 1 {
		t.Fatalf("same term, interval not passed: %v", ts.log())
	}
	ts.term = 7 // lost and regained between two rounds
	s.leaderStep(ctx)
	if count() != 2 {
		t.Fatalf("a new term without a new barrier: %v", ts.log())
	}
}

// D11: how long a removed node's heartbeat is still accepted through the
// heartbeat fast path's copy of the Raft configuration (at most
// raftnet.CacheAge old), measured from the moment the member has the
// configuration without it.
func TestRemovedHeartbeatBound(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	m := follower(h, x)
	// The removed node's service is stopped so that only the test speaks
	// with its key.
	h.stop(x)
	stop := make(chan struct{})
	type probe struct {
		at time.Time
		ok bool
	}
	results := make(chan probe, 100000)
	go func() {
		for {
			select {
			case <-stop:
				close(results)
				return
			default:
			}
			err := raftnet.SendHeartbeat(x.key.Certificate, m.cfg.PeerAddress(), m.key.Pin(), x.id, x.cfg.PeerAddress(), 1, 2*time.Second)
			results <- probe{at: time.Now(), ok: err == nil}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	cid := leader.svc.machine.View().ClusterID
	if _, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: x.name, AllowUnanswered: true, ClusterID: cid}, "root"); err != nil {
		t.Fatal(err)
	}
	// When the member has the configuration without x.
	var effective time.Time
	h.eventually(m.name+" has the configuration without "+x.name, 60*time.Second, func() bool {
		servers, err := m.st.Servers()
		if err != nil {
			return false
		}
		if _, in := findServer(servers, x.id); in {
			return false
		}
		effective = time.Now()
		return true
	})
	time.Sleep(time.Second)
	close(stop)
	var lastOK time.Time
	accepted := 0
	for p := range results {
		if p.ok {
			accepted++
			lastOK = p.at
		}
	}
	if accepted == 0 {
		t.Fatal("no heartbeat was accepted before the removal: the probe does not work")
	}
	late := lastOK.Sub(effective)
	t.Logf("last accepted heartbeat %s after %s had the configuration without %s (bound %s)", late.Round(time.Millisecond), m.name, x.name, raftnet.CacheAge)
	// The poll that saw the change may itself be up to 50 ms late.
	if late > raftnet.CacheAge+100*time.Millisecond {
		t.Fatalf("a heartbeat was accepted %s after the change took effect", late)
	}
}

// leave --wipe: a crash after each file step, then a service start (refused
// while the marker exists) and a rerun that finishes without asking: a
// one-node cluster with the marker's new cluster ID, a new key, the same
// node ID, no address. The rerun after the last step is a no-op.
func TestWipeCrashPoints(t *testing.T) {
	points := []string{"", wipeStopAfterStandalone, wipeStopAfterJoin, wipeStopAfterState, wipeStopAfterKey, wipeStopAfterCluster}
	for _, point := range points {
		name := point
		if name == "" {
			name = "no crash"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			n := h.node("smith")
			h.start(n)
			h.eventually("smith leads", 20*time.Second, func() bool { return n.svc.machine.View().ClusterID != "" })
			oldCluster, oldPin := n.svc.machine.View().ClusterID, n.key.Pin()
			h.stop(n)
			// Leftovers a wipe deletes: a moved-aside state and join.json.
			if err := os.Mkdir(filepath.Join(n.dir, "standalone.20260101T000000Z"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(n.dir, joinstate.FileName), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			id, resume, err := WipeInfo(n.dir, n.id)
			if err != nil || id != oldCluster || resume {
				t.Fatalf("wipe info: %s %v %v", id, resume, err)
			}
			m, err := WipeBegin(n.dir, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			cfg := n.cfg
			cfg.ListenAddress = "" // root's step between begin and finish
			if point != "" {
				_, _, err := wipeFinish(context.Background(), n.dir, cfg, time.Now(), func(p string) bool { return p == point })
				if err != errStopped {
					t.Fatalf("stop at %q: %v", point, err)
				}
				if err := StartRule(n.dir, discardLog()); err == nil || !strings.Contains(err.Error(), "leave --wipe") {
					t.Fatalf("a start with the marker: %v", err)
				}
				id, resume, err := WipeInfo(n.dir, n.id)
				if err != nil || !resume || id != m.NewClusterID {
					t.Fatalf("wipe info after the crash: %s %v %v", id, resume, err)
				}
				if again, err := WipeBegin(n.dir, time.Now()); err != nil || again.NewClusterID != m.NewClusterID {
					t.Fatalf("a second begin: %+v %v", again, err)
				}
			}
			cid, pin, err := WipeFinish(context.Background(), n.dir, cfg, time.Now())
			if err != nil || cid != m.NewClusterID || pin == oldPin {
				t.Fatalf("finish: %s %s %v", cid, pin, err)
			}
			for _, left := range []string{layout.WipeMarker, joinstate.FileName, "standalone.20260101T000000Z"} {
				if ok, _ := layout.Exists(filepath.Join(n.dir, left)); ok {
					t.Fatalf("%s is left", left)
				}
			}
			n.cfg = cfg
			n.key = loadKey(t, n.dir)
			h.start(n)
			h.eventually("the new one-node cluster", 20*time.Second, func() bool { return n.st.IsLeader() && n.svc.machine.View().ClusterID == cid })
			v := n.svc.machine.View()
			if len(v.Members) != 1 || v.Members[0].ID != n.id || v.Members[0].Address != "" || v.Members[0].Pin != pin {
				t.Fatalf("after the wipe: %+v", v)
			}
			if n.svc.Status(context.Background()).Listening {
				t.Fatal("a listener after the wipe")
			}
		})
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func loadKey(t *testing.T, dir string) *nodekey.Key {
	t.Helper()
	k, err := nodekey.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// join --abandon: a crash after each file step, then a service start
// (refused in phase abandoning) and a rerun that finishes without asking
// the members again (they are all stopped by then).
func TestAbandonCrashPoints(t *testing.T) {
	for _, point := range []string{abandonStopAfterPhase, abandonStopAfterDelete, abandonStopAfterRename} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			h.cluster("smith", "langrishe")
			leader := h.leader()
			j := h.node("wu")
			h.start(j)
			h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
			own := j.svc.machine.View().ClusterID
			// A join that the cluster refuses after an unknown outcome is
			// not needed: the admission's member accepts the key until
			// the pending entry is withdrawn, so withdraw it first; then
			// every member answers and refuses the key.
			adm := h.admit(leader, j)
			h.stop(j)
			if _, err := joinstate.MoveAside(j.dir, "20260101T000000Z"); err != nil {
				t.Fatal(err)
			}
			if err := joinstate.Write(j.dir, joinstate.State{Phase: joinstate.PhaseUnknown, OpID: newID(t), Admission: adm, Standalone: "standalone.20260101T000000Z"}); err != nil {
				t.Fatal(err)
			}
			if err := layout.MakeState(j.dir); err != nil { // the joining node's empty state
				t.Fatal(err)
			}
			if _, err := leader.svc.Withdraw(context.Background(), j.name, "root"); err != nil {
				t.Fatal(err)
			}
			for _, n := range h.running() {
				n := n
				h.eventually(n.name+" refuses wu's key", 10*time.Second, func() bool { return refusedIn(n, j) })
			}
			out := stageAbandon(j.dir, j.cfg, j.key, 3*time.Second, false, func(p string) bool { return p == point })
			if out.Outcome != "stopped" {
				t.Fatalf("stop at %q: %+v", point, out)
			}
			if err := StartRule(j.dir, discardLog()); err == nil || !strings.Contains(err.Error(), "join --abandon") {
				t.Fatalf("a start in phase abandoning: %v", err)
			}
			// The members are stopped now: a rerun must not ask them.
			for _, n := range h.running() {
				h.stop(n)
			}
			out = StageAbandon(j.dir, j.cfg, j.key, time.Second)
			if out.Outcome != "restored" || !out.Resumed || len(out.Probes) != 0 {
				t.Fatalf("rerun: %+v", out)
			}
			h.start(j)
			h.eventually("wu's own cluster is back", 20*time.Second, func() bool {
				return j.st.IsLeader() && j.svc.machine.View().ClusterID == own
			})
		})
	}
}

// join --abandon refusals: no join in progress; no node answers.
func TestAbandonRefusals(t *testing.T) {
	h := newHarness(t)
	n := h.node("smith")
	if out := StageAbandon(n.dir, n.cfg, n.key, time.Second); out.Outcome != "no_join" {
		t.Fatalf("without a join: %+v", out)
	}
	_, _, gn := ghost(t, "ghost", "127.0.88.9")
	adm := joincode.Admission{ClusterID: newID(t), For: joincode.Node{NodeID: n.id, Name: n.name, Address: n.cfg.PeerAddress(), Pin: n.key.Pin()},
		Members: []joincode.Node{gn}, ExpiresAt: 1}
	if _, err := joinstate.MoveAside(n.dir, "20260101T000000Z"); err != nil {
		t.Fatal(err)
	}
	if err := joinstate.Write(n.dir, joinstate.State{Phase: joinstate.PhaseAccepted, OpID: newID(t), Admission: adm, Standalone: "standalone.20260101T000000Z"}); err != nil {
		t.Fatal(err)
	}
	out := StageAbandon(n.dir, n.cfg, n.key, time.Second)
	if out.Outcome != "no_answer" {
		t.Fatalf("nobody answers: %+v", out)
	}
	if js, _ := joinstate.Read(n.dir); js == nil || js.Phase != joinstate.PhaseAccepted {
		t.Fatal("a refused abandon changed join.json")
	}
}

// The start rule (D1, D2): an earlier build's layout is refused with the
// paths to move; a leftover moved-aside state of a member that is not
// joining is deleted with a log line.
func TestStartRuleLayoutAndLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "raft.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := StartRule(dir, discardLog())
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "state")) || !strings.Contains(err.Error(), filepath.Join(dir, "membership.json")) {
		t.Fatalf("old layout: %v", err)
	}
	h := newHarness(t)
	h.cluster("smith", "langrishe")
	n := h.running()[1]
	h.stop(n)
	if err := os.Mkdir(filepath.Join(n.dir, "standalone.20260101T000000Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.start(n)
	h.eventually("the leftover deleted", 20*time.Second, func() bool { return len(standalones(t, n.dir)) == 0 })
	if !strings.Contains(n.logs.String(), "a moved-aside one-node state left by a finished join was deleted") {
		t.Fatal("no log line")
	}
}

// D10: once an answer is accepted, a joining node accepts the answer's
// nodes only; in phase refused it accepts no key; the answer's addresses
// get the admission's port check.
func TestJoiningNodeAcceptedKeys(t *testing.T) {
	_, _, a := ghost(t, "a", "127.0.88.10")
	_, _, b := ghost(t, "b", "127.0.88.11")
	self := newID(t)
	s := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "self"}})
	s.join = &joinstate.State{Phase: joinstate.PhaseUnknown, Admission: joincode.Admission{Members: []joincode.Node{a}}}
	if _, ok := s.ByPin(a.Pin); !ok {
		t.Fatal("the admission's node is not accepted before an answer")
	}
	s.join.Phase, s.join.Answer = joinstate.PhaseAccepted, []joincode.Node{b}
	if _, ok := s.ByPin(a.Pin); ok {
		t.Fatal("the admission's node is still accepted once the answer replaced it")
	}
	if _, ok := s.ByPin(b.Pin); !ok {
		t.Fatal("the answer's node is not accepted")
	}
	s.join.Phase = joinstate.PhaseRefused
	if _, ok := s.ByPin(b.Pin); ok || s.acceptedNow().source != "none (the join was refused)" {
		t.Fatal("a key is accepted in phase refused")
	}
	bad := b
	bad.Address = "127.0.88.11:9444"
	if err := checkAnswer(JoinAnswer{ClusterID: "c", Members: []joincode.Node{bad}}, "c"); err == nil || !strings.Contains(err.Error(), "9444") {
		t.Fatalf("an answer with port 9444: %v", err)
	}
}
