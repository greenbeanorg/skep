package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// reloadKey loads n's key as serve does at start (LoadRepair: a key
// renamed before its certificate was issued gets one).
func reloadKey(t *testing.T, n *tnode) {
	t.Helper()
	k, _, err := nodekey.LoadRepair(n.dir, n.id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n.key = k
}

func healthCheck(n *tnode, name string) string {
	return n.svc.Health(context.Background()).Checks[name].Error
}

// D1: with one member down the rotation waits, naming it; the rotating
// node keeps its old key; when the member returns, everything finishes by
// itself.
func TestRotationMemberDown(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	y := follower(h, x)
	h.stop(y)
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	for _, n := range []*tnode{leader, x} {
		if p, next := pinOn(n, x); p != res.OldPin || next != res.NewPin {
			t.Fatalf("%s: %s %s", n.name, p, next)
		}
		if c := healthCheck(n, "key_rotation"); !strings.Contains(c, "waits for "+y.name) {
			t.Fatalf("%s health: %q", n.name, c)
		}
	}
	if x.key.Pin() != res.OldPin {
		t.Fatal("x switched while a member had not applied the new pin")
	}
	st := x.svc.Status(context.Background())
	if st.Rotation == nil || st.Rotation.Using != res.OldPin {
		t.Fatalf("status rotation %+v", st.Rotation)
	}
	start := time.Now()
	h.start(y)
	h.rotated(x, res.NewPin, 90*time.Second)
	t.Logf("the rotation finished %s after %s returned", time.Since(start).Round(time.Second), y.name)
}

// D1: abort while waiting: the next pin goes, the pin stays, the files go;
// the member that was down sees the old pin only.
func TestRotationAbort(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	y := follower(h, x)
	h.stop(y)
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.AbortRotation(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the next pin is gone and the files deleted", 30*time.Second, func() bool {
		for _, n := range []*tnode{leader, x} {
			if p, next := pinOn(n, x); p != res.OldPin || next != "" {
				return false
			}
		}
		return x.svc.rotationState() == nil && !fileExists(x.dir, nodekey.NextKeyFile) && !fileExists(x.dir, layout.RotateFile)
	})
	if _, err := x.svc.AbortRotation(context.Background(), "root"); opCode(err) != "no_rotation" {
		t.Fatalf("a second abort: %v", err)
	}
	h.start(y)
	h.eventually(y.name+" has the old pin only", 30*time.Second, func() bool {
		p, next := pinOn(y, x)
		return p == res.OldPin && next == ""
	})
}

// D1: an abort is refused once the node uses its new key.
func TestRotationAbortAfterSwitchRefused(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	x := follower(h)
	stopped := make(chan struct{}, 1)
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool {
		if step == rotStopAfterSwitch {
			stopped <- struct{}{}
			return true
		}
		return false
	}
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("no switch")
	}
	if _, err := x.svc.AbortRotation(context.Background(), "root"); opCode(err) != "refused" || !strings.Contains(err.Error(), "already uses its new key") {
		t.Fatalf("abort after the switch: %v", err)
	}
	// The leader refuses an abort from the new key too (the transport
	// gives the member entry's pin as Pin and the key used as ConnPin).
	leader := h.leader()
	_, err = leader.svc.keyAbortAsLeader(raftnet.Peer{NodeID: x.id, Name: x.name, Pin: res.OldPin, ConnPin: res.NewPin, Kind: raftnet.KindMember},
		keyAbortRequest{OpID: res.OpID, NextPin: res.NewPin, Actor: "root"})
	if err == nil || !strings.Contains(err.Error(), "already uses its new key") {
		t.Fatalf("leader: %v", err)
	}
}

// D1: removing the member that is down during a waiting rotation: the
// removal finishes without it, and the rotation then finishes by itself.
func TestRotationRemoveDownMember(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	y := follower(h, x)
	h.stop(y)
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.remove(leader, y.name)
	h.eventually(y.name+" is gone", 60*time.Second, func() bool { _, ok := leader.svc.machine.Member(y.id); return !ok })
	h.rotated(x, res.NewPin, 60*time.Second)
}

// D1: the rotating node itself is removed during its rotation (here it is
// stopped from switching): the removal finishes and takes both pins.
func TestRotationRemoveRotatingNode(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterAdd }
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.remove(leader, x.name)
	h.eventually(x.name+" is gone with both pins", 60*time.Second, func() bool {
		for _, n := range h.running() {
			if n == x {
				continue
			}
			if _, ok := n.svc.machine.Member(x.id); ok {
				return false
			}
		}
		return true
	})
	for _, n := range h.running() {
		if n == x {
			continue
		}
		if _, ok := n.svc.ByPin(res.NewPin); ok {
			t.Fatalf("%s still accepts the next pin", n.name)
		}
		if _, ok := n.svc.ByPin(res.OldPin); ok {
			t.Fatalf("%s still accepts the pin", n.name)
		}
	}
}

// D1: the leader stopped right after the add entry: nobody has seen it
// apply the add, so the rotation waits for it (a member that is down);
// once it is back, the new leader finishes the rotation.
func TestRotationLeaderStoppedAfterAdd(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.stop(leader)
	h.eventually("a new leader", 30*time.Second, func() bool { return h.leader() != nil })
	time.Sleep(2 * time.Second)
	if c := healthCheck(h.leader(), "key_rotation"); !strings.Contains(c, x.name) {
		t.Fatalf("the new leader's health: %q", c)
	}
	h.start(leader)
	h.rotated(x, res.NewPin, 90*time.Second)
}

// D1: the leader stopped right after it applied the done entry: the
// others apply it from the log.
func TestRotationLeaderStoppedAfterDone(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("the leader applied the done entry", 60*time.Second, func() bool {
		p, next := pinOn(leader, x)
		return p == res.NewPin && next == ""
	})
	h.stop(leader)
	h.rotated(x, res.NewPin, 60*time.Second)
	h.start(leader)
	h.rotated(x, res.NewPin, 60*time.Second)
}

// D1: the leader rotates its own key.
func TestRotationOfTheLeader(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	res, err := leader.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.rotated(leader, res.NewPin, 90*time.Second)
}

// D1: refusals. An admission pending refuses a rotation (and the files go
// again); a rotation refuses an admission and a second rotation.
func TestRotationRefusals(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	y := follower(h, x)
	line, _, _ := ghost(t, "ghost", "127.0.88.9")
	if _, err := leader.svc.Admit(context.Background(), line, "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("x has the pending entry", 20*time.Second, func() bool { return len(x.svc.machine.View().Pendings) == 1 })
	if _, err := x.svc.Rotate(context.Background(), "root"); opCode(err) != "refused" || !strings.Contains(err.Error(), "admission is pending") {
		t.Fatalf("rotation with an admission pending: %v", err)
	}
	if fileExists(x.dir, nodekey.NextKeyFile) || fileExists(x.dir, layout.RotateFile) {
		t.Fatal("a refused rotation left files")
	}
	if _, err := leader.svc.Withdraw(context.Background(), "ghost", "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("withdrawn on x", 20*time.Second, func() bool { return len(x.svc.machine.View().Pendings) == 0 })
	// The leader also refuses a key-add for a pending admission (raced).
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterAdd }
	x.svc.mu.Unlock()
	if _, err := x.svc.Rotate(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.svc.Admit(context.Background(), line, "root"); opCode(err) != "rotation_in_progress" || !strings.Contains(err.Error(), x.name) {
		t.Fatalf("admit during a rotation: %v", err)
	}
	if _, err := y.svc.Rotate(context.Background(), "root"); opCode(err) != "rotation_in_progress" || !strings.Contains(err.Error(), x.name) {
		t.Fatalf("a second rotation: %v", err)
	}
	if _, err := x.svc.Rotate(context.Background(), "root"); opCode(err) != "rotation_in_progress" {
		t.Fatalf("a second rotation on the same node: %v", err)
	}
	// A key-add from another node's key for x is impossible: the leader
	// takes the node from the key. A member key asking for a pin that is
	// a member's is refused.
	_, err := leader.svc.keyAddAsLeader(raftnet.Peer{NodeID: y.id, Name: y.name, Pin: y.key.Pin(), Kind: raftnet.KindMember},
		keyAddRequest{OpID: "y-op", NextPin: leader.key.Pin(), Actor: "root"})
	if opCode(err) != "rotation_in_progress" {
		t.Fatalf("key-add during a rotation: %v", err)
	}
	_, err = leader.svc.keyAddAsLeader(raftnet.Peer{NodeID: y.id, Name: y.name, Pin: y.key.Pin(), Kind: raftnet.KindPending},
		keyAddRequest{OpID: "y-op2", NextPin: leader.key.Pin(), Actor: "root"})
	if opCode(err) != "refused" {
		t.Fatalf("key-add from a pending key: %v", err)
	}
}

// D1: a member restarted while a next pin exists keeps it (the durable
// applied-membership file holds next_pin, format 2) and accepts both keys
// as the rotating node; the rotation then finishes.
func TestRotationRestartsInTheMiddle(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterAdd }
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range h.running() {
		if n == x {
			continue
		}
		h.eventually(n.name+" has the next pin", 20*time.Second, func() bool { _, next := pinOn(n, x); return next == res.NewPin })
		h.stop(n)
		f, err := membership.Read(n.state())
		if err != nil || f.Format != 2 {
			t.Fatalf("%s: membership.json %+v %v", n.name, f, err)
		}
		found := false
		for _, m := range f.Members {
			found = found || (m.ID == x.id && m.NextPin == res.NewPin)
		}
		if !found {
			t.Fatalf("%s: no next pin in membership.json", n.name)
		}
		h.start(n)
		if p, ok := n.svc.ByPin(res.NewPin); !ok || p.NodeID != x.id {
			t.Fatalf("%s after its restart: the next pin is not %s", n.name, x.name)
		}
		if p, ok := n.svc.ByPin(res.OldPin); !ok || p.NodeID != x.id {
			t.Fatalf("%s after its restart: the pin is not %s", n.name, x.name)
		}
		h.eventually(n.name+" has a leader again", 30*time.Second, func() bool { return h.leader() != nil })
	}
	// The rotating node itself restarts: its rotation goes on.
	h.stop(x)
	h.start(x)
	h.rotated(x, res.NewPin, 90*time.Second)
}

// D1: every crash point of steps 1 to 5 and of the abort. The node is
// stopped at the point and started again; the start rule and the
// rotation step finish (or undo) it.
func TestRotationCrashPoints(t *testing.T) {
	for _, point := range []string{rotStopAfterNext, rotStopAfterRecord, rotStopAfterAdd, rotStopAfterRename, rotStopAfterSwitch} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			defer h.stopAll()
			h.cluster("smith", "langrishe")
			x := follower(h)
			oldPin := x.key.Pin()
			x.svc.mu.Lock()
			x.svc.afterRotateStep = func(step string) bool { return step == point }
			x.svc.mu.Unlock()
			res, err := x.svc.Rotate(context.Background(), "root")
			early := point == rotStopAfterNext || point == rotStopAfterRecord
			if early != (err != nil) {
				t.Fatalf("rotate: %+v %v", res, err)
			}
			if !early {
				h.eventually("the crash point", 30*time.Second, func() bool { return x.svc.isFrozen() })
			}
			h.stop(x)
			reloadKey(t, x)
			h.start(x)
			if point == rotStopAfterNext {
				// node.key.next without rotate.json: deleted at start.
				if fileExists(x.dir, nodekey.NextKeyFile) || x.svc.rotationState() != nil || x.key.Pin() != oldPin {
					t.Fatal("after a crash before rotate.json")
				}
				return
			}
			r, _ := ReadRotate(x.dir)
			if r == nil {
				t.Fatal("rotate.json is gone")
			}
			h.rotated(x, r.NewPin, 90*time.Second)
			if x.key.Pin() != r.NewPin {
				t.Fatalf("x uses %s", x.key.Pin())
			}
		})
	}
	for _, point := range []string{rotStopAfterAbort, rotStopAfterNextRm} {
		t.Run("abort "+point, func(t *testing.T) {
			h := newHarness(t)
			defer h.stopAll()
			h.cluster("smith", "langrishe", "star")
			x := follower(h)
			y := follower(h, x)
			h.stop(y) // the rotation waits
			oldPin := x.key.Pin()
			res, err := x.svc.Rotate(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			x.svc.mu.Lock()
			x.svc.afterRotateStep = func(step string) bool { return step == point }
			x.svc.mu.Unlock()
			_, err = x.svc.AbortRotation(context.Background(), "root")
			if point == rotStopAfterAbort && err != errStopped {
				t.Fatalf("abort: %v", err)
			}
			h.eventually("the crash point", 30*time.Second, func() bool { return x.svc.isFrozen() })
			h.stop(x)
			reloadKey(t, x)
			h.start(x)
			h.eventually("the abort finished", 60*time.Second, func() bool {
				p, next := pinOn(x, x)
				return p == oldPin && next == "" && x.svc.rotationState() == nil &&
					!fileExists(x.dir, nodekey.NextKeyFile) && !fileExists(x.dir, layout.RotateFile)
			})
			if x.key.Pin() != oldPin {
				t.Fatal("the key changed")
			}
			_ = res
		})
	}
}

// D1: a refused rotation that crashed between its two deletes: the start
// rule refuses and names the three files.
func TestRotationStartRuleRefusal(t *testing.T) {
	dir := t.TempDir()
	if _, err := nodekey.Create(dir, "00000000-0000-4000-8000-000000000001", time.Now()); err != nil {
		t.Fatal(err)
	}
	old, _ := nodekey.FilePin(dir, nodekey.KeyFile)
	r := RotateState{OldPin: old, NewPin: "sha256:" + strings.Repeat("e", 64), OpID: "op", Started: "x", Phase: PhaseRotating}
	if err := writeRotate(dir, r); err != nil {
		t.Fatal(err)
	}
	err := RotateStartRule(dir, discardLog().Warn)
	if err == nil || !strings.Contains(err.Error(), layout.RotateFile) || !strings.Contains(err.Error(), nodekey.NextKeyFile) || !strings.Contains(err.Error(), nodekey.KeyFile) {
		t.Fatalf("start rule: %v", err)
	}
	// In phase aborting the same files finish the abort.
	r.Phase = PhaseAborting
	_ = writeRotate(dir, r)
	if err := RotateStartRule(dir, discardLog().Warn); err != nil || fileExists(dir, layout.RotateFile) {
		t.Fatalf("aborting: %v", err)
	}
	// node.key.next without rotate.json is deleted.
	if _, err := nodekey.CreateNext(dir); err != nil {
		t.Fatal(err)
	}
	if err := RotateStartRule(dir, discardLog().Warn); err != nil || fileExists(dir, nodekey.NextKeyFile) {
		t.Fatalf("next without record: %v", err)
	}
}

// D1: a wipe during a rotation deletes node.key.next and rotate.json; the
// node is removed on a member, and the next pin goes with its entry.
func TestRotationWipe(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterAdd }
	x.svc.mu.Unlock()
	if _, err := x.svc.Rotate(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	h.stop(x)
	if _, err := WipeBegin(x.dir, x.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := x.cfg
	cfg.ListenAddress = ""
	if _, _, err := WipeFinish(context.Background(), x.dir, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if fileExists(x.dir, nodekey.NextKeyFile) || fileExists(x.dir, layout.RotateFile) {
		t.Fatal("the wipe left rotation files")
	}
	if _, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: x.name}, "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("x and its next pin are gone", 60*time.Second, func() bool {
		_, ok := leader.svc.machine.Member(x.id)
		return !ok
	})
}
