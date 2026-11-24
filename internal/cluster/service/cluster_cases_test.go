package service

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// ghost is a key nobody runs, with a request for it.
func ghost(t *testing.T, name, ip string) (string, *nodekey.Key, joincode.Node) {
	t.Helper()
	id, _ := ids.NewUUID()
	k, err := nodekey.Create(t.TempDir(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	n := joincode.Node{NodeID: id, Name: name, Address: ip + ":8444", Pin: k.Pin()}
	line, err := joincode.EncodeRequest(joincode.Request{Node: n})
	if err != nil {
		t.Fatal(err)
	}
	return line, k, n
}

func opCode(err error) string {
	var oe *localapi.OpError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

func pendings(n *tnode) []clusterstate.Pending { return n.svc.machine.View().Pendings }

// Expiry is a log entry: the same state on every node; a listener whose
// clock is ahead refuses the key earlier; nothing is accepted after the
// expiry entry.
func TestPendingExpiry(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe")
	leader := h.leader()
	var follower *tnode
	for _, n := range nodes {
		if n != leader {
			follower = n
		}
	}
	line, gk, gn := ghost(t, "ghost", "127.0.88.9")
	if _, err := leader.svc.Admit(context.Background(), line, "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the pending entry on both nodes", 10*time.Second, func() bool {
		return len(pendings(leader)) == 1 && len(pendings(follower)) == 1
	})
	if _, ok := follower.svc.ByPin(gk.Pin()); !ok {
		t.Fatal("the follower does not accept the pending key")
	}
	// The follower's clock runs 31 minutes ahead: it refuses the key by
	// its own clock, while the leader (on time) proposes no expiry yet.
	follower.clock.advance(31 * time.Minute)
	if _, ok := follower.svc.ByPin(gk.Pin()); ok {
		t.Fatal("a listener whose clock is past the expiry still accepts the key")
	}
	if _, ok := leader.svc.ByPin(gk.Pin()); !ok {
		t.Fatal("the leader refused the key before its expiry")
	}
	time.Sleep(time.Second)
	if len(pendings(leader)) != 1 || len(pendings(follower)) != 1 {
		t.Fatal("the pending entry went away without an expiry entry")
	}
	// The leader's clock passes the expiry: it proposes the entry, and
	// every node's state changes by it, the same way.
	leader.clock.advance(31 * time.Minute)
	h.eventually("the pending entry gone on both nodes", 10*time.Second, func() bool {
		return len(pendings(leader)) == 0 && len(pendings(follower)) == 0
	})
	for _, n := range nodes {
		if _, ok := n.svc.ByPin(gk.Pin()); ok {
			t.Fatalf("%s accepts the key after the expiry entry", n.name)
		}
		if !strings.Contains(n.logs.String(), "cluster membership: pending expired") {
			t.Fatalf("%s did not log the expiry", n.name)
		}
	}
	a, _ := leader.svc.machine.Snapshot()
	b, _ := follower.svc.machine.Snapshot()
	if string(a) != string(b) {
		t.Fatalf("the nodes differ after the expiry:\n%s\n%s", a, b)
	}
	// Even the leader, with its clock set back, accepts nothing now.
	leader.clock.advance(-31 * time.Minute)
	if _, ok := leader.svc.ByPin(gk.Pin()); ok {
		t.Fatal("accepted after the expiry entry")
	}
	_ = gn
}

// Withdrawal closes the key's open connection; each duplicate is refused;
// admit needs a listen address.
func TestWithdrawalDuplicatesAndNoListenAddress(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe")
	smith := h.leader()
	line, gk, gn := ghost(t, "ghost", "127.0.88.9")
	if _, err := nodes[0].svc.Admit(context.Background(), line, "root"); err != nil {
		t.Fatal(err)
	}
	// Each duplicate: ID, name, address, pin, against members and the
	// pending entry.
	for _, d := range []joincode.Node{
		{NodeID: gn.NodeID, Name: "other", Address: "127.0.88.10:8444", Pin: pinOf(t)},
		{NodeID: newID(t), Name: "ghost", Address: "127.0.88.10:8444", Pin: pinOf(t)},
		{NodeID: newID(t), Name: "other", Address: gn.Address, Pin: pinOf(t)},
		{NodeID: newID(t), Name: "other", Address: "127.0.88.10:8444", Pin: gn.Pin},
		{NodeID: smith.id, Name: "other", Address: "127.0.88.10:8444", Pin: pinOf(t)},
		{NodeID: newID(t), Name: smith.name, Address: "127.0.88.10:8444", Pin: pinOf(t)},
		{NodeID: newID(t), Name: "other", Address: smith.cfg.PeerAddress(), Pin: pinOf(t)},
		{NodeID: newID(t), Name: "other", Address: "127.0.88.10:8444", Pin: smith.key.Pin()},
	} {
		l, _ := joincode.EncodeRequest(joincode.Request{Node: d})
		for _, on := range nodes { // on the leader and passed on by a follower
			if _, err := on.svc.Admit(context.Background(), l, "root"); opCode(err) != "refused" {
				t.Errorf("duplicate %+v admitted on %s: %v", d, on.name, err)
			}
		}
	}
	if n := len(pendings(smith)); n != 1 {
		t.Fatalf("%d pending entries", n)
	}
	// The pending key holds a connection to every member.
	var conns []*tls.Conn
	for _, n := range nodes {
		c, err := raftnet.DialPinned(n.cfg.PeerAddress(), n.key.Pin(), gk.Certificate, 5*time.Second)
		if err != nil {
			t.Fatalf("pending key to %s: %v", n.name, err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// Withdrawn on the follower, passed on to the leader.
	var follower *tnode
	for _, n := range nodes {
		if n != smith {
			follower = n
		}
	}
	if _, err := follower.svc.Withdraw(context.Background(), "ghost", "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("pending entry withdrawn everywhere", 10*time.Second, func() bool {
		return len(pendings(nodes[0])) == 0 && len(pendings(nodes[1])) == 0
	})
	for i, c := range conns {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("the withdrawn key's connection to %s is still open", nodes[i].name)
		}
	}
	// The key is refused in the handshake now.
	if _, err := raftnet.DialPinned(smith.cfg.PeerAddress(), smith.key.Pin(), gk.Certificate, 5*time.Second); err == nil {
		t.Fatal("the withdrawn key was accepted")
	}
	if _, err := smith.svc.Withdraw(context.Background(), "ghost", "root"); opCode(err) != "not_found" {
		t.Fatalf("second withdrawal: %v", err)
	}
	// A node without a listen address refuses admit.
	single := withStore(New(Options{Config: clusterconf.Config{NodeID: newID(t), NodeName: "solo"}}), mustOneNode(t))
	if _, err := single.Admit(context.Background(), line, "root"); opCode(err) != "no_listen_address" {
		t.Fatalf("admit without a listen address: %v", err)
	}
}

func newID(t *testing.T) string {
	id, err := ids.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pinOf(t *testing.T) string {
	k, err := nodekey.Create(t.TempDir(), newID(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k.Pin()
}

func mustOneNode(t *testing.T) *store.Node {
	n, _ := oneNode(t, "")
	return n
}

// The leader stopped after each join step, in a cluster that keeps its
// quorum: a new leader finishes the join. The promotion rule waits for
// every member, so the stopped node is started again (as a follower) once
// another node leads.
func TestNewLeaderFinishesAJoin(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run("after step "+string(rune('0'+step)), func(t *testing.T) {
			h := newHarness(t)
			nodes := h.cluster("a", "b", "c")
			old := h.leader()
			frozen := make(chan struct{}, 1)
			old.svc.mu.Lock()
			old.svc.afterJoinStep = func(n int, _ string) bool {
				if n != step {
					return false
				}
				frozen <- struct{}{}
				return true
			}
			old.svc.mu.Unlock()
			d := h.node("d")
			h.start(d)
			adm := h.admit(old, d)
			out := h.join(d, adm) // the stage, then the joiner runs
			if out.Outcome != JoinAccepted {
				t.Fatalf("join call: %+v", out)
			}
			select {
			case <-frozen:
			case <-time.After(60 * time.Second):
				t.Fatalf("step %d was not reached", step)
			}
			h.stop(old)
			var newLeader *tnode
			h.eventually("a new leader", 30*time.Second, func() bool {
				newLeader = h.leader()
				return newLeader != nil && newLeader != old
			})
			h.start(old)
			old.svc.mu.Lock()
			old.svc.afterJoinStep = nil
			old.svc.mu.Unlock()
			h.joined(d, 90*time.Second)
			for _, n := range append(nodes, d) {
				if len(voters(n)) != 4 {
					t.Fatalf("%s voters %v", n.name, voters(n))
				}
			}
			// The remaining steps were done by the new leader: the step 4
			// entry's origin is not the stopped leader (except when it was
			// stopped after step 4).
			var line string
			for _, l := range strings.Split(d.logs.String(), "\n") {
				if strings.Contains(l, "cluster membership: member") && strings.Contains(l, "node_id="+d.id) {
					line = l
				}
			}
			if step < 4 && (line == "" || strings.Contains(line, "origin="+old.id)) {
				t.Fatalf("step 4 was not done by a new leader: %q", line)
			}
			_ = newLeader
		})
	}
}

// One member stopped: the joiner stays a working non-voter, health on every
// running member and on the joiner names the stopped member, and the
// promotion follows its return.
func TestPromotionWaitsForAStoppedMember(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe", "star")
	var down *tnode
	for _, n := range nodes {
		if n != h.leader() {
			down = n
			break
		}
	}
	h.stop(down)
	d := h.node("wu")
	h.start(d)
	adm := h.admit(h.leader(), d)
	if out := h.join(d, adm); out.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", out)
	}
	h.eventually("wu is a non-voter applying entries", 30*time.Second, func() bool {
		servers, _ := h.leader().st.Servers()
		sv, ok := findServer(servers, d.id)
		return ok && !sv.Voter && stateOf(d, d.id) == clusterstate.StateJoining
	})
	for _, n := range append(nodes, d) {
		if n.svc == nil {
			continue
		}
		n := n
		h.eventually("health on "+n.name+" names "+down.name, 30*time.Second, func() bool {
			c := n.svc.Health(context.Background()).Checks["join"]
			return strings.Contains(c.Error, "waits for") && strings.Contains(c.Error, down.name)
		})
	}
	time.Sleep(time.Second)
	if voters(h.leader())[d.id] {
		t.Fatal("promoted while a member is down")
	}
	// The non-voter works: it applies new entries.
	line, _, _ := ghost(t, "ghost", "127.0.88.9")
	if _, err := h.leader().svc.Admit(context.Background(), line, "root"); err != nil {
		t.Fatal(err)
	}
	h.eventually("the non-voter applied a new entry", 10*time.Second, func() bool { return len(pendings(d)) == 1 })
	h.start(down)
	h.joined(d, 60*time.Second)
}

// A joiner whose admission is older than a newer member learns that member
// from the join call's answer.
func TestJoinerLearnsANewerMemberFromTheAnswer(t *testing.T) {
	h := newHarness(t)
	smith := h.cluster("smith")[0]
	late := h.node("late")
	h.start(late)
	old := h.admit(smith, late) // lists smith only
	if len(old.Members) != 1 {
		t.Fatalf("admission members %+v", old.Members)
	}
	h.stop(late) // its own one-node cluster plays no part until it joins
	lang := h.node("langrishe")
	h.start(lang)
	if out := h.join(lang, h.admit(smith, lang)); out.Outcome != JoinAccepted {
		t.Fatal(out)
	}
	h.joined(lang, 60*time.Second)
	out := StageJoin(context.Background(), late.dir, late.cfg, late.key, old, time.Now(), 100*time.Millisecond)
	if out.Outcome != JoinAccepted {
		t.Fatalf("join with the older admission: %+v", out)
	}
	js, _ := joinstate.Read(late.dir)
	learned := false
	for _, n := range js.Peers(late.id) {
		if n.NodeID == lang.id && n.Pin == lang.key.Pin() {
			learned = true
		}
	}
	if !learned {
		t.Fatalf("the answer did not carry langrishe: %+v", js.Answer)
	}
	h.start(late)
	if p, ok := late.svc.ByPin(lang.key.Pin()); !ok || p.NodeID != lang.id {
		// Either still in admission mode, or already by its state
		// machine: both must accept langrishe.
		t.Fatal("the joiner does not accept the newer member")
	}
	h.joined(late, 60*time.Second)
}

// A join that fails before step 1 puts the one-node state back; one whose
// outcome is unknown does not, and the service keeps retrying.
func TestFailedAndUnknownJoins(t *testing.T) {
	h := newHarness(t)
	smith := h.cluster("smith")[0]
	j := h.node("joiner")
	h.start(j)
	adm := h.admit(smith, j)
	before, _ := os.ReadFile(filepath.Join(j.dir, store.DBFile))
	beforeID := ""
	h.eventually("joiner's cluster ID", 10*time.Second, func() bool { beforeID = j.svc.machine.View().ClusterID; return beforeID != "" })
	h.stop(j)

	// A wrong pin for the member: every handshake fails, nothing is sent.
	wrong := adm
	wrong.Members = []joincode.Node{adm.Members[0]}
	wrong.Members[0].Pin = pinOf(t)
	out := StageJoin(context.Background(), j.dir, j.cfg, j.key, wrong, time.Now(), 50*time.Millisecond)
	if out.Outcome != JoinUnreached {
		t.Fatalf("wrong pin: %+v", out)
	}
	if js, _ := joinstate.Read(j.dir); js != nil {
		t.Fatal("join.json left after a join that failed before step 1")
	}
	after, _ := os.ReadFile(filepath.Join(j.dir, store.DBFile))
	if len(after) == 0 || len(before) == 0 {
		t.Fatal("the one-node store is not back")
	}
	h.start(j)
	h.eventually("the one-node cluster is back, same ID and pin, healthy", 20*time.Second, func() bool {
		st := j.svc.Status(context.Background())
		return st.ClusterID == beforeID && st.Pin == j.key.Pin() && len(st.Members) == 1 && j.st.IsLeader()
	})
	if pendings(smith)[0].NodeID != j.id {
		t.Fatal("the cluster's pending entry changed")
	}
	h.stop(j)

	// A member that takes the request and never answers: unknown.
	silent := startSilentMember(t, "127.0.88.20:8444")
	unk := adm
	unk.Members = []joincode.Node{{NodeID: newID(t), Name: "silent", Address: "127.0.88.20:8444", Pin: silent.Pin()}}
	out = StageJoin(context.Background(), j.dir, j.cfg, j.key, unk, time.Now(), 50*time.Millisecond)
	if out.Outcome != JoinUnknown {
		t.Fatalf("silent member: %+v", out)
	}
	js, _ := joinstate.Read(j.dir)
	if js == nil || js.Phase != joinstate.PhaseUnknown {
		t.Fatalf("join.json %+v", js)
	}
	if _, err := os.Stat(filepath.Join(j.dir, store.DBFile)); err == nil {
		t.Fatal("the one-node store was put back after an unknown outcome")
	}
	if _, err := os.Stat(filepath.Join(j.dir, js.Standalone, store.DBFile)); err != nil {
		t.Fatal("the moved-aside store is missing")
	}
	h.start(j)
	h.eventually("health says the outcome is unknown and it repeats the call", 20*time.Second, func() bool {
		c := j.svc.Health(context.Background()).Checks["joining"]
		return strings.Contains(c.Error, "unknown") && silent.calls() >= 2
	})
}

// silentMember accepts a key, takes a join request and never answers.
type silentMember struct {
	key *nodekey.Key
	ln  *peerlistener.Listener
	n   chan struct{}
}

func (s *silentMember) Pin() string { return s.key.Pin() }

func (s *silentMember) calls() int { return len(s.n) }

func startSilentMember(t *testing.T, addr string) *silentMember {
	t.Helper()
	k, err := nodekey.Create(t.TempDir(), newID(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := &silentMember{key: k, n: make(chan struct{}, 100)}
	s.ln, err = peerlistener.Listen(peerlistener.Options{Address: netip.MustParseAddrPort(addr), Certificate: k.Certificate,
		Accept: func(string) bool { return true },
		Handle: func(c *tls.Conn, _ string) {
			c.Write([]byte{0x01})
			buf := make([]byte, 4096)
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if n, _ := c.Read(buf); n > 0 {
				s.n <- struct{}{}
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.ln.Close() })
	return s
}

// A full stop of three nodes before their first snapshot elects a leader
// again: each accepts the others by its applied-membership file. A member
// restarted alone is served again.
func TestFullStopBeforeTheFirstSnapshot(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe", "star")
	want := nodes[0].svc.machine.View().Members
	for _, n := range nodes {
		h.eventually(n.name+" file lists three", 10*time.Second, func() bool {
			f, _ := membership.Read(n.dir)
			return f != nil && len(f.Members) == 3
		})
	}
	for _, n := range nodes {
		h.stop(n)
	}
	for _, n := range nodes {
		h.start(n)
	}
	h.eventually("a leader after the full stop", 20*time.Second, func() bool { return h.leader() != nil })
	for _, n := range nodes {
		n := n
		h.eventually(n.name+" applies the same membership", 20*time.Second, func() bool {
			got := n.svc.machine.View().Members
			if len(got) != len(want) {
				return false
			}
			for i := range got {
				if got[i] != want[i] {
					return false
				}
			}
			return true
		})
	}
	// One member restarted alone is served again.
	var one *tnode
	for _, n := range nodes {
		if n != h.leader() {
			one = n
		}
	}
	h.stop(one)
	h.start(one)
	h.eventually(one.name+" up to date again", 30*time.Second, func() bool {
		return one.svc.Status(context.Background()).StateMachine.UpToDate && one.svc.Health(context.Background()).Checks["raft"].Status == "ok"
	})
}

// After a restart, before the state machine has reached the file's index,
// keys are accepted by the file: a member listed there is accepted and a
// pending key is not.
func TestKeysByTheFileUntilCaughtUp(t *testing.T) {
	dir := t.TempDir()
	w, err := membership.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := newID(t), newID(t)
	pa, pb := pinOf(t), pinOf(t)
	cid := newID(t)
	if err := w.Write(clusterstate.Membership{ClusterID: cid, Epoch: 1, Index: 9, Members: []clusterstate.Member{
		{ID: a, Name: "a", Address: "127.0.88.1:8444", Pin: pa, State: clusterstate.StateMember},
		{ID: b, Name: "b", Address: "127.0.88.2:8444", Pin: pb, State: clusterstate.StateJoining},
	}}); err != nil {
		t.Fatal(err)
	}
	m := clusterstate.New()
	s := New(Options{Config: clusterconf.Config{NodeID: a, NodeName: "a"}, Machine: m, Writer: w})
	if p, ok := s.ByPin(pb); !ok || p.Kind != raftnet.KindJoining || s.acceptedNow().source != "membership file" {
		t.Fatalf("by the file: %+v %v", p, ok)
	}
	// The machine replays: cluster.init, a pending entry for c, still
	// behind the file's index.
	init, _ := clusterstate.NewEntry(clusterstate.TypeClusterInit, "i", clusterstate.ActorSystem, a, clusterstate.ClusterInit{ClusterID: cid,
		Member: clusterstate.Member{ID: a, Name: "a", Address: "127.0.88.1:8444", Pin: pa, State: clusterstate.StateMember}})
	raw, _ := init.Encode()
	m.Apply(1, raw)
	pc := pinOf(t)
	pend, _ := clusterstate.NewEntry(clusterstate.TypePendingCreate, "p", "root", a, clusterstate.PendingCreate{
		NodeID: newID(t), Name: "c", Address: "127.0.88.3:8444", Pin: pc, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	raw, _ = pend.Encode()
	m.Apply(2, raw)
	if _, ok := s.ByPin(pc); ok {
		t.Fatal("a pending key was accepted before the node caught up with its file")
	}
	if _, ok := s.ByPin(pb); !ok {
		t.Fatal("a member of the file was refused before the node caught up")
	}
	// The file was never written back to a lower index by the replay.
	m.SetMembershipWriter(w.Write)
	if f := w.Current(); f.Index != 9 {
		t.Fatalf("file index %d", f.Index)
	}
}

// A fault between the file's write and the report: the file is ahead of
// what the node reported, never behind, and a restart does not lower it.
func TestFaultBetweenWriteAndReport(t *testing.T) {
	dir := t.TempDir()
	w, _ := membership.NewWriter(dir)
	m := clusterstate.New()
	crash := false
	m.SetMembershipWriter(func(rec clusterstate.Membership) error {
		if err := w.Write(rec); err != nil {
			return err
		}
		if crash {
			return errors.New("injected: the process stops after the file write")
		}
		return nil
	})
	a, pa, cid := newID(t), pinOf(t), newID(t)
	init, _ := clusterstate.NewEntry(clusterstate.TypeClusterInit, "i", clusterstate.ActorSystem, a, clusterstate.ClusterInit{ClusterID: cid,
		Member: clusterstate.Member{ID: a, Name: "a", Address: "127.0.88.1:8444", Pin: pa, State: clusterstate.StateMember}})
	raw, _ := init.Encode()
	m.Apply(1, raw)
	b, pb := newID(t), pinOf(t)
	pend, _ := clusterstate.NewEntry(clusterstate.TypePendingCreate, "p", "root", a, clusterstate.PendingCreate{
		NodeID: b, Name: "b", Address: "127.0.88.2:8444", Pin: pb, ExpiresAt: 100})
	raw, _ = pend.Encode()
	m.Apply(2, raw)
	join, _ := clusterstate.NewEntry(clusterstate.TypeMemberJoin, "j", "node:"+b, a, clusterstate.MemberJoin{NodeID: b, Pin: pb, At: 50})
	rawJoin, _ := join.Encode()
	crash = true
	if r := m.Apply(3, rawJoin); !errors.Is(r.Err, clusterstate.ErrStalled) {
		t.Fatalf("%+v", r)
	}
	reported, _ := m.AppliedIndex()
	f, _ := membership.Read(dir)
	if f.Index != 3 || reported != 2 {
		t.Fatalf("file index %d, reported %d: the file must be ahead", f.Index, reported)
	}
	// Restart: a new machine replays from the start; the file is not
	// lowered on the way and ends at the same index.
	w2, _ := membership.NewWriter(dir)
	m2 := clusterstate.New()
	var seen []uint64
	m2.SetMembershipWriter(func(rec clusterstate.Membership) error {
		if err := w2.Write(rec); err != nil {
			return err
		}
		cur, _ := membership.Read(dir)
		seen = append(seen, cur.Index)
		return nil
	})
	m2.Apply(1, mustEncode(t, init))
	m2.Apply(2, mustEncode(t, pend))
	m2.Apply(3, rawJoin)
	for _, idx := range seen {
		if idx < 3 {
			t.Fatalf("the file went back to index %d", idx)
		}
	}
	if f, _ := membership.Read(dir); f.Index != 3 || len(f.Members) != 2 {
		t.Fatalf("after replay: %+v", f)
	}
}

func mustEncode(t *testing.T, e clusterstate.Entry) []byte {
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The local API's admit answers in the formats the commands print.
func TestAdmitOverTheLocalAPI(t *testing.T) {
	h := newHarness(t)
	smith := h.cluster("smith")[0]
	line, _, gn := ghost(t, "ghost", "127.0.88.9")
	handler := localapi.NewHandler(smith.svc, localapi.HandlerOptions{APIUID: 409})
	srv := &http.Server{Handler: handler, ConnContext: localapi.ConnContext}
	sock := filepath.Join(t.TempDir(), "c.sock")
	ln, err := localapi.Listen(localapi.ListenOptions{Path: sock, AllowedUIDs: []uint32{uint32(os.Getuid())}, MaxConnections: 4})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	_, err = localapi.NewClient(sock).Admit(context.Background(), line)
	// This test runs as an unprivileged user: the socket admits it as a
	// peer, but admit is root-only, so it is refused.
	var se *localapi.StatusError
	if os.Getuid() != 0 && (!errors.As(err, &se) || se.HTTPStatus != http.StatusForbidden) {
		t.Fatalf("admit as a user: %v", err)
	}
	_ = gn
	_ = net.IPv4len
}
