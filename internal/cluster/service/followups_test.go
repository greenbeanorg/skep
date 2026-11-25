package service

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// Step 8b-2b, job 2b: the hand-over target is a voter in state member that
// answered the status round, the one with the highest applied index.
func TestHandoverTarget(t *testing.T) {
	const self, a, b, c, d = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-00000000000a",
		"00000000-0000-4000-8000-00000000000b", "00000000-0000-4000-8000-00000000000c", "00000000-0000-4000-8000-00000000000d"
	view := clusterstate.View{Members: []clusterstate.Member{
		{ID: self, State: clusterstate.StateRemoving},
		{ID: a, State: clusterstate.StateMember},
		{ID: b, State: clusterstate.StateMember},
		{ID: c, State: clusterstate.StateRemoving},
		{ID: d, State: clusterstate.StateJoining},
	}}
	servers := []store.Server{{ID: self, Voter: true}, {ID: a, Voter: true, Address: "a"}, {ID: b, Voter: true, Address: "b"},
		{ID: c, Voter: true}, {ID: d, Voter: false}}
	answers := map[string]statusResult{
		a: {applied: 10}, b: {applied: 12}, c: {applied: 99}, d: {applied: 99}, self: {applied: 99},
	}
	got, ok := handoverTarget(view, servers, answers, self)
	if !ok || got.ID != b {
		t.Fatalf("target %+v %v, want b (highest applied of the members)", got, ok)
	}
	answers[b] = statusResult{err: errors.New("no answer")}
	if got, ok = handoverTarget(view, servers, answers, self); !ok || got.ID != a {
		t.Fatalf("target %+v %v, want a (b did not answer)", got, ok)
	}
	answers[a] = statusResult{applied: 10, stalled: true}
	if got, ok = handoverTarget(view, servers, answers, self); ok {
		t.Fatalf("target %+v: a stalled member, a removing one and a joining one are never targets", got)
	}
	// A tie goes to the lowest ID.
	answers[a], answers[b] = statusResult{applied: 7}, statusResult{applied: 7}
	if got, _ = handoverTarget(view, servers, answers, self); got.ID != a {
		t.Fatalf("tie: %s", got.ID)
	}
}

// Job 2b: the leader that removes itself with no voter in state member to
// hand over to is refused with handover_failed and writes nothing. Two
// nodes; the other one's removal is frozen after step 0, so it is a voter
// in state removing.
func TestRemoveSelfWithoutTargetRefused(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
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
	before := leader.svc.machine.View().AppliedIndex
	_, err := leader.svc.Remove(context.Background(), localapi.RemoveRequest{Name: leader.name}, "root")
	if opCode(err) != "handover_failed" || !strings.Contains(err.Error(), "no other voter in state member") {
		t.Fatalf("remove self without a target: %v", err)
	}
	if after := leader.svc.machine.View().AppliedIndex; after != before || !leader.st.IsLeader() {
		t.Fatalf("something changed: applied %d -> %d, leader %v", before, after, leader.st.IsLeader())
	}
}

// Job 2e: admit, withdraw and remove on a removed node that was joining
// name join --abandon.
func TestRemovedJoiningNodeNamesAbandon(t *testing.T) {
	s := New(Options{Config: clusterconf.Config{NodeID: "00000000-0000-4000-8000-000000000001", NodeName: "x", ListenAddress: "127.0.0.1"},
		Removed: &RemovedRecord{ClusterID: "c"}, Logger: discardLog()})
	_, err := s.Remove(context.Background(), localapi.RemoveRequest{Name: "y"}, "root")
	if err == nil || !strings.Contains(err.Error(), "leave --wipe") {
		t.Fatalf("not joining: %v", err)
	}
	s.join = &joinstate.State{}
	for _, err := range []error{
		func() error { _, err := s.Remove(context.Background(), localapi.RemoveRequest{Name: "y"}, "root"); return err }(),
		func() error { _, err := s.Admit(context.Background(), "x", "root"); return err }(),
		func() error { _, err := s.Withdraw(context.Background(), "y", "root"); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "join --abandon") || strings.Contains(err.Error(), "leave --wipe") {
			t.Fatalf("joining: %v", err)
		}
	}
}

// Job 2f: removal_applied says nothing until the first status round since
// the service started has ended, by the injected clock.
func TestRemovalAppliedWaitsForFirstRound(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	self := "00000000-0000-4000-8000-000000000001"
	other := "00000000-0000-4000-8000-000000000002"
	s := New(Options{Config: clusterconf.Config{NodeID: self, NodeName: "self"}, Now: clock, Logger: discardLog(), LeaderInterval: time.Minute})
	view := clusterstate.View{RemovedIndex: 5, Members: []clusterstate.Member{
		{ID: self, Name: "self", State: clusterstate.StateMember},
		{ID: other, Name: "other", State: clusterstate.StateMember},
	}}
	if got := s.unappliedRemoval(view); got != nil {
		t.Fatalf("before the first round: %v", got)
	}
	s.statusStep(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		done := s.firstRound
		s.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first round did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := s.unappliedRemoval(view)
	if len(got) != 2 || !strings.Contains(got[1], "other (no answer since this node started)") {
		t.Fatalf("after the first round: %v", got)
	}
	// The next round is due only after LeaderInterval by the clock.
	s.mu.Lock()
	next := s.nextStatus
	s.mu.Unlock()
	if !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("next round at %v", next)
	}
}

// Job 2d: CreateCluster, which only commands run (init, the last step of a
// wipe), writes nothing to stderr: the library's start-up lines would
// reach the administrator's terminal.
func TestCreateClusterIsQuiet(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	dir := t.TempDir()
	id := "00000000-0000-4000-8000-0000000000aa"
	_, _, cerr := CreateCluster(context.Background(), dir, clusterconf.Config{NodeID: id, NodeName: "quiet"}, "", time.Now())
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(out) != 0 {
		t.Fatalf("CreateCluster wrote to stderr: %q", out)
	}
}
