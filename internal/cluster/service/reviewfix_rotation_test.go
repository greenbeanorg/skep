package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// nextKeyOf loads x's node.key.next with a certificate, from a copy.
func nextKeyOf(t *testing.T, x *tnode) *nodekey.Key {
	t.Helper()
	dir := t.TempDir()
	copyFile(t, filepath.Join(x.dir, nodekey.NextKeyFile), filepath.Join(dir, nodekey.KeyFile))
	k, _, err := nodekey.LoadRepair(dir, x.id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// F2 of the 8b-2b review: a refused key-add is an abort. Nothing is
// deleted on the refusal alone: phase aborting is recorded, the abort asks
// the leader, and node.key.next goes only once the rotation has ended
// keeping the old key (the first attempt's entry is in the log and
// applies) or the leader has said the pin was never added. The refusal is
// injected (rotationRefused called with a reason), because a leader whose
// view lacks an applied add cannot be produced on purpose in-process.
func TestRefusedKeyAddIsAnAbort(t *testing.T) {
	t.Run("the first attempt's entry applies", func(t *testing.T) {
		h := newHarness(t)
		defer h.stopAll()
		h.cluster("smith", "langrishe", "star")
		x := follower(h)
		y := follower(h, x)
		h.stop(y) // the rotation cannot switch: it stays at the add
		oldPin := x.key.Pin()
		res, err := x.svc.Rotate(context.Background(), "root")
		if err != nil || res.AddIndex == 0 {
			t.Fatalf("rotate: %+v %v", res, err)
		}
		h.eventually("x applied its add", 20*time.Second, func() bool { _, next := pinOn(x, x); return next == res.NewPin })
		r := x.svc.rotationState()
		// node.key.next is kept while x's member entry still has the
		// next pin.
		stop := make(chan struct{})
		watched := make(chan struct{})
		bad := make(chan string, 1)
		svc := x.svc
		go func() {
			defer close(watched)
			for {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
				if m, _ := svc.machine.Member(x.id); m.NextPin == res.NewPin && !fileExists(x.dir, nodekey.NextKeyFile) {
					select {
					case bad <- "node.key.next deleted while the next pin is in x's member entry":
					default:
					}
				}
			}
		}()
		x.svc.rotationRefused(context.Background(), *r, "an injected refusal")
		h.eventually("the rotation ended keeping the old key", 60*time.Second, func() bool {
			p, next := pinOn(x, x)
			return p == oldPin && next == "" && x.svc.rotationState() == nil &&
				!fileExists(x.dir, nodekey.NextKeyFile) && !fileExists(x.dir, layout.RotateFile)
		})
		close(stop)
		<-watched
		select {
		case msg := <-bad:
			t.Fatal(msg)
		default:
		}
		types := map[string]int{}
		for _, rec := range records(t, x) {
			if rec.NodeID == x.id {
				types[rec.EntryType]++
			}
		}
		if types[clusterstate.TypeMemberKeyDone] != 1 {
			t.Fatalf("x's audit: %v", types)
		}
		if x.key.Pin() != oldPin {
			t.Fatal("the key changed")
		}
		h.start(y)
	})
	t.Run("never added", func(t *testing.T) {
		h := newHarness(t)
		defer h.stopAll()
		h.cluster("smith", "langrishe")
		x := follower(h)
		// Steps 1 and 2 only, as if the first key-add were refused.
		x.svc.mu.Lock()
		x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterRecord }
		x.svc.mu.Unlock()
		if _, err := x.svc.Rotate(context.Background(), "root"); !errors.Is(err, errStopped) {
			t.Fatalf("rotate: %v", err)
		}
		r := x.svc.rotationState()
		x.svc.mu.Lock()
		x.svc.afterRotateStep, x.svc.frozen = nil, false
		x.svc.mu.Unlock()
		x.svc.rotationRefused(context.Background(), *r, "an injected refusal")
		if x.svc.rotationState() != nil || fileExists(x.dir, nodekey.NextKeyFile) || fileExists(x.dir, layout.RotateFile) {
			t.Fatal("the leader said not added, and the files are left")
		}
	})
}

// F2: the crash points of the refusal path finish by themselves at the
// next start: after phase aborting is recorded (the abort is asked again),
// and after node.key.next is deleted (the start rule deletes rotate.json).
func TestRefusedKeyAddCrashPoints(t *testing.T) {
	for _, point := range []string{rotStopAfterAbort, rotStopAfterNextRm} {
		t.Run(point, func(t *testing.T) {
			h := newHarness(t)
			defer h.stopAll()
			h.cluster("smith", "langrishe", "star")
			x := follower(h)
			y := follower(h, x)
			h.stop(y)
			oldPin := x.key.Pin()
			res, err := x.svc.Rotate(context.Background(), "root")
			if err != nil {
				t.Fatal(err)
			}
			h.eventually("x applied its add", 20*time.Second, func() bool { _, next := pinOn(x, x); return next == res.NewPin })
			x.svc.mu.Lock()
			x.svc.afterRotateStep = func(step string) bool { return step == point }
			x.svc.mu.Unlock()
			x.svc.rotationRefused(context.Background(), *x.svc.rotationState(), "an injected refusal")
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
			h.start(y)
		})
	}
}

// F3 of the 8b-2b review: the rename is lost after the leader dropped the
// old pin (the files are put back to their state before the rename; the
// applied-membership file keeps only the new pin). At the next start the
// node switches by itself and is accepted again.
func TestRotationLostRename(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	old := t.TempDir()
	copyFile(t, filepath.Join(x.dir, nodekey.KeyFile), filepath.Join(old, nodekey.KeyFile))
	copyFile(t, filepath.Join(x.dir, nodekey.CertFile), filepath.Join(old, nodekey.CertFile))
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterSwitch }
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	// The leader drops the old pin, and x applies that (its rotation
	// step is frozen after the switch, its Raft instance is not).
	h.eventually("x applied keep new", 60*time.Second, func() bool {
		p, next := pinOn(x, x)
		return p == res.NewPin && next == ""
	})
	h.stop(x)
	if f, err := membership.Read(x.state()); err != nil || !fileListsPin(f, x.id, res.NewPin) {
		t.Fatalf("membership.json: %v", err)
	}
	// The rename did not survive: node.key.next is the new key again,
	// node.key and its certificate the old ones.
	copyFile(t, filepath.Join(x.dir, nodekey.KeyFile), filepath.Join(x.dir, nodekey.NextKeyFile))
	copyFile(t, filepath.Join(old, nodekey.KeyFile), filepath.Join(x.dir, nodekey.KeyFile))
	copyFile(t, filepath.Join(old, nodekey.CertFile), filepath.Join(x.dir, nodekey.CertFile))
	reloadKey(t, x)
	if x.key.Pin() != res.OldPin {
		t.Fatal("setup: node.key is not the old key")
	}
	h.start(x)
	h.rotated(x, res.NewPin, 90*time.Second)
	if x.key.Pin() != res.NewPin || fileExists(x.dir, nodekey.NextKeyFile) {
		t.Fatalf("x uses %s", x.key.Pin())
	}
	h.eventually("health ok on x", 30*time.Second, func() bool { return x.svc.Health(context.Background()).Status == "ok" })
}

func fileListsPin(f *membership.File, id, pin string) bool {
	for _, m := range f.Members {
		if m.ID == id {
			return m.Pin == pin && m.NextPin == ""
		}
	}
	return false
}

// F3: the switch is durable before the new key is used. With the data
// directory's fsync failing after the rename, the node keeps its old key
// in memory; once the fsync works it adopts the new one (node.key.next is
// gone, node.key has the new pin: sync, then adopt).
func TestRotationSwitchWaitsForTheSync(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	x := follower(h)
	failing := make(chan struct{})
	x.svc.mu.Lock()
	x.svc.syncDir = func(dir string) error {
		select {
		case <-failing:
			return layout.SyncDir(dir)
		default:
			return errors.New("an injected fsync failure")
		}
	}
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("the rename happened", 30*time.Second, func() bool {
		pin, _ := nodekey.FilePin(x.dir, nodekey.KeyFile)
		return pin == res.NewPin && !fileExists(x.dir, nodekey.NextKeyFile)
	})
	time.Sleep(500 * time.Millisecond) // several rotation rounds
	if x.key.Pin() != res.OldPin {
		t.Fatal("the new key is used before the fsync succeeded")
	}
	close(failing)
	h.rotated(x, res.NewPin, 60*time.Second)
	if x.key.Pin() != res.NewPin {
		t.Fatalf("x uses %s", x.key.Pin())
	}
}

// F3: an abort after the rename is refused, also when the new key's
// certificate could not be issued (the node still uses the old key in
// memory): the decision is made from the disk.
func TestAbortAfterTheRenameIsRefused(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	x := follower(h)
	x.svc.mu.Lock()
	x.svc.afterRotateStep = func(step string) bool { return step == rotStopAfterRename }
	x.svc.mu.Unlock()
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("the crash point", 30*time.Second, func() bool { return x.svc.isFrozen() })
	if x.key.Pin() != res.OldPin {
		t.Fatal("setup: x already uses its new key")
	}
	_, err = x.svc.AbortRotation(context.Background(), "root")
	if opCode(err) != "refused" || !strings.Contains(err.Error(), "already uses its new key") {
		t.Fatalf("abort after the rename: %v", err)
	}
	if r, _ := ReadRotate(x.dir); r == nil || r.Phase != PhaseRotating {
		t.Fatalf("rotate.json changed: %+v", r)
	}
}

// F4 of the 8b-2b review: once the leader has seen a node present its new
// key, a later connection with the old key does not undo it.
func TestSeenNewKeyIsRemembered(t *testing.T) {
	id := "00000000-0000-4000-8000-0000000000b2"
	oldPin, newPin := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	s := New(Options{Config: clusterconf.Config{NodeID: "00000000-0000-4000-8000-0000000000a1"}, Logger: discardLog()})
	m := clusterstate.Member{ID: id, Pin: oldPin, NextPin: newPin}
	s.SawPin(id, oldPin)
	if s.seenNewKey(m) {
		t.Fatal("seen with the old key only")
	}
	s.SawPin(id, newPin)
	s.SawPin(id, oldPin) // anyone holding the old key reconnects
	if !s.seenNewKey(m) {
		t.Fatal("a connection with the old key undid the new key's")
	}
	if got := s.keysSeen(clusterstate.View{Members: []clusterstate.Member{m}}); len(got) != 1 || got[0].Pin != oldPin {
		t.Fatalf("status shows the latest pin seen: %+v", got)
	}
	// A later rotation's next pin is not seen by the earlier one.
	if s.seenNewKey(clusterstate.Member{ID: id, Pin: newPin, NextPin: "sha256:" + strings.Repeat("c", 64)}) {
		t.Fatal("another next pin counted as seen")
	}
}

// F4, in a cluster: after the node was seen with its new key, connections
// with the old key to the leader do not delay member.key_done.
func TestOldKeyConnectionsDoNotDelayKeyDone(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	y := follower(h, x)
	leader := h.leader()
	oldKey := loadKey(t, x.dir)
	h.stop(y) // the rotation waits at the add
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	// The leader sees the new key (as after x's switch).
	newKey := nextKeyOf(t, x)
	if r, _ := raftnet.ProbeKey(leader.cfg.PeerAddress(), leader.key.Pin(), newKey.Certificate, 3*time.Second); r != raftnet.ProbeAccepted {
		t.Fatalf("the new key: %s", r)
	}
	leader.svc.SawPin(x.id, res.NewPin)
	// Old-key connections keep coming.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if leader.svc != nil {
				leader.svc.SawPin(x.id, oldKey.Pin())
			}
		}
	}()
	h.start(y)
	h.rotated(x, res.NewPin, 90*time.Second)
	close(stop)
	<-done
}

// F5 of the 8b-2b review: the leader refuses a key-abort sent over a
// connection made with the node's new key, and writes nothing.
func TestKeyAbortWithTheNewKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	y := follower(h, x)
	h.stop(y) // the rotation stays at the add
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	leader := h.leader()
	h.eventually("the leader applied the add", 20*time.Second, func() bool { _, next := pinOn(leader, x); return next == res.NewPin })
	r := x.svc.rotationState()
	newKey := nextKeyOf(t, x)
	caller := raftnet.Caller{NodeID: x.id, Certificate: newKey.Certificate}
	ctx, cancel := context.WithTimeout(context.Background(), raftnet.PeerTimeout)
	defer cancel()
	var ans keyAbortAnswer
	err = caller.Call(ctx, leader.cfg.PeerAddress(), leader.key.Pin(), raftnet.OpKeyAbort, keyAbortRequest{OpID: r.OpID, NextPin: r.NewPin, Actor: "root"}, &ans)
	var pe *raftnet.PeerError
	if !errors.As(err, &pe) || pe.Code != raftnet.CodeRefused || !strings.Contains(pe.Message, "already uses its new key") {
		t.Fatalf("key-abort with the new key: %+v %v", ans, err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, next := pinOn(leader, x); next != res.NewPin {
		t.Fatal("the next pin was dropped")
	}
	m, _ := leader.svc.machine.Member(x.id)
	if _, ok := leader.svc.machine.OpIndex(clusterstate.KeyDoneOpID(x.id, m.KeyAddIndex, clusterstate.KeepOld)); ok {
		t.Fatal("a member.key_done keep old was written")
	}
	h.start(y)
}

// F9 of the 8b-2b review: a status round before the state machine has
// applied anything asks the members of the applied-membership file with
// both pins of a rotating member.
func TestRoundMembersFromTheFileKeepTheNextPin(t *testing.T) {
	dir := t.TempDir()
	w, err := membership.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-0000000000b2"
	next := "sha256:" + strings.Repeat("b", 64)
	if err := w.Write(clusterstate.Membership{ClusterID: "00000000-0000-4000-8000-0000000000cc", Epoch: 1, Index: 5,
		Members: []clusterstate.Member{{ID: id, Name: "smith", Address: "127.0.0.2:8444", Pin: "sha256:" + strings.Repeat("a", 64),
			NextPin: next, State: clusterstate.StateMember}}}); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Config: clusterconf.Config{NodeID: "00000000-0000-4000-8000-0000000000a1"}, Writer: w, Logger: discardLog()})
	if m := s.roundMembers(); len(m) != 1 || m[0].NextPin != next {
		t.Fatalf("round members: %+v", m)
	}
}

// V2 of the 8b-2b review: a leader whose barrier of the current term has
// not yet succeeded answers the peer operations (join, admit, withdraw,
// remove, key-add, key-abort) with "unavailable", never with a refusal
// computed from a view that may lack earlier terms' entries.
func TestLeaderAnswersOnlyAfterTheTermBarrier(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	l := h.leader()
	x := follower(h)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	l.svc.mu.Lock()
	l.svc.beforeBarrier = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	l.svc.barrierTerm = 0 // as at the start of a term
	l.svc.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader did not start its barrier")
	}
	from := raftnet.Peer{NodeID: x.id, Name: x.name, Pin: x.key.Pin(), ConnPin: x.key.Pin(), Kind: raftnet.KindMember}
	for op, body := range map[string]string{
		raftnet.OpJoin:     `{"op_id":"00000000-0000-4000-8000-0000000000dd"}`,
		raftnet.OpAdmit:    `{"request":"x","actor":"root","op_id":"op1"}`,
		raftnet.OpWithdraw: `{"name":"wu","actor":"root","op_id":"op2"}`,
		raftnet.OpRemove:   `{"name":"wu","actor":"root","op_id":"op3"}`,
		raftnet.OpKeyAdd:   `{"op_id":"op4","next_pin":"sha256:` + strings.Repeat("e", 64) + `","actor":"root"}`,
		raftnet.OpKeyAbort: `{"op_id":"op5","next_pin":"sha256:` + strings.Repeat("e", 64) + `","actor":"root"}`,
		// Step 8c-1: users-write computes no refusal before the barrier.
		raftnet.OpUsersWrite: `{"type":"user.delete","op_id":"op6","actor":"local:0","data":{"id":"00000000-0000-4000-8000-0000000000ee"}}`,
	} {
		_, err := l.svc.HandlePeer(context.Background(), op, from, []byte(body))
		var pe *raftnet.PeerError
		if !errors.As(err, &pe) || pe.Code != raftnet.CodeUnavailable || !strings.Contains(pe.Message, "just become the leader") {
			t.Errorf("%s before the barrier: %v", op, err)
		}
	}
	l.svc.mu.Lock()
	l.svc.beforeBarrier = nil
	l.svc.mu.Unlock()
	close(release)
	h.eventually("the leader answers after its barrier", 10*time.Second, func() bool { _, not := l.svc.notLeader(); return !not })
}

// V3 of the 8b-2b review: at the apply of member.key_done, a connection
// accepted with the dropped pin is closed (well before the listener's
// 5-minute idle limit), and the pin is refused in the next handshake.
func TestKeyDoneClosesConnectionsOfTheDroppedPin(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	n := follower(h, x)
	oldKey := loadKey(t, x.dir)
	h.stop(n) // the rotation waits at the add
	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.start(n)
	h.eventually(n.name+" has the next pin", 20*time.Second, func() bool { _, next := pinOn(n, x); return next == res.NewPin })
	// A connection to n with the old key, while both pins are accepted.
	c, err := raftnet.DialPinned(n.cfg.PeerAddress(), n.key.Pin(), oldKey.Certificate, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	closed := make(chan time.Time, 1)
	early := make(chan string, 1)
	svc := n.svc
	go func() {
		buf := make([]byte, 16)
		for {
			if _, err := c.Read(buf); err != nil {
				// Closed only once n has dropped the old pin.
				if m, ok := svc.machine.Member(x.id); !ok || m.Pin != res.NewPin || m.NextPin != "" {
					early <- fmt.Sprintf("closed before keep new was applied: %+v %v", m, err)
				}
				closed <- time.Now()
				return
			}
		}
	}()
	h.rotated(x, res.NewPin, 90*time.Second)
	applied := time.Now() // n has applied keep new by now (rotated checks it)
	select {
	case at := <-closed:
		select {
		case msg := <-early:
			t.Fatal(msg)
		default:
		}
		if at.After(applied.Add(5 * time.Second)) {
			t.Fatalf("closed %s after the apply", at.Sub(applied))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the connection with the dropped pin is still open")
	}
	if r, _ := raftnet.ProbeKey(n.cfg.PeerAddress(), n.key.Pin(), oldKey.Certificate, 3*time.Second); r != raftnet.ProbeRefused {
		t.Fatalf("the dropped pin in the next handshake: %s", r)
	}
}
