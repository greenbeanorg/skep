package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// D3: the clock check from each node's point of view, with injected clocks
// only. A member 8 s ahead is a warning, 20 s ahead is critical; a member
// without a sample is "not known" and no warning; nothing is refused.
func TestClockHealth(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	h.eventually("every clock known on the leader, within the limits", 20*time.Second, func() bool {
		cs := leader.svc.Status(context.Background()).Clocks
		if len(cs) != 2 {
			return false
		}
		for _, c := range cs {
			if !c.Known || c.RoundTripMillis > 2000 {
				return false
			}
		}
		return leader.svc.Health(context.Background()).Checks["clock"].Error == ""
	})
	x.clock.advance(8 * time.Second)
	h.eventually("a warning naming x", 20*time.Second, func() bool {
		c := leader.svc.Health(context.Background()).Checks["clock"]
		return strings.Contains(c.Error, x.name+"'s clock is +8.") && !c.Critical
	})
	// From x's point of view the others are 8 s behind.
	h.eventually("x sees the others behind", 20*time.Second, func() bool {
		c := x.svc.Health(context.Background()).Checks["clock"]
		return strings.Contains(c.Error, leader.name+"'s clock is -7.") || strings.Contains(c.Error, leader.name+"'s clock is -8.")
	})
	x.clock.advance(12 * time.Second)
	h.eventually("critical above 15 s", 20*time.Second, func() bool {
		c := leader.svc.Health(context.Background()).Checks["clock"]
		return c.Critical && c.Status == "unavailable" && strings.Contains(c.Error, x.name+"'s clock is +20.")
	})
	// Nothing is refused because of a clock: a write still goes through.
	e, _ := clusterstate.NewEntry(clusterstate.TypePendingWithdraw, "clock-w", "root", leader.id, clusterstate.PendingWithdraw{NodeID: x.id, Name: "nobody"})
	if _, err := leader.st.Apply(e); opCode(applyError(err)) != "refused" {
		t.Fatalf("a write with a skewed member: %v", err)
	}
	x.clock.advance(-20 * time.Second)
	h.eventually("ok again", 20*time.Second, func() bool {
		return leader.svc.Health(context.Background()).Checks["clock"].Error == ""
	})
}

// D3, mixed builds: a node that does not know the clock operation answers
// "forbidden"; its clock is "not known (older build or refused)", that is
// no warning, and its status answers still count (nothing reads "no
// answer").
func TestClockOlderBuild(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	leader := h.leader()
	// The other node answers an operation it does not know exactly as a
	// build before 8b-2b answers "clock": forbidden.
	leader.svc.mu.Lock()
	leader.svc.clockOp = "clock-of-a-later-build"
	leader.svc.clocks = map[string]clockSample{}
	leader.svc.mu.Unlock()
	h.eventually("the other node's clock reads not known (older build)", 20*time.Second, func() bool {
		cs := leader.svc.Status(context.Background()).Clocks
		return len(cs) == 1 && !cs[0].Known && strings.Contains(cs[0].Note, "older build")
	})
	// The status round still has an answer from it.
	leader.svc.mu.Lock()
	answered := 0
	for id, r := range leader.svc.answers {
		if id != leader.id && r.err == nil {
			answered++
		}
	}
	leader.svc.mu.Unlock()
	if answered != 1 {
		t.Fatalf("the status answer of an older build must still count: %d", answered)
	}
	if c := leader.svc.Health(context.Background()).Checks["clock"]; c.Error != "" {
		t.Fatalf("not known is no warning: %s", c.Error)
	}
}

// D3: the sample arithmetic, and a slow sample is dropped.
func TestClockSampleArithmetic(t *testing.T) {
	c := clockSample{ok: true, offset: 6 * time.Second, uncertainty: 1500 * time.Millisecond}
	if c.excess() != 4500*time.Millisecond {
		t.Fatal(c.excess())
	}
	c = clockSample{ok: true, offset: -6 * time.Second, uncertainty: 500 * time.Millisecond}
	if c.excess() != 5500*time.Millisecond {
		t.Fatal(c.excess())
	}
	if (clockSample{offset: time.Second, uncertainty: 2 * time.Second}).excess() != 0 {
		t.Fatal("excess below zero")
	}
	s := New(Options{Logger: discardLog()})
	s.cfg.NodeID = "00000000-0000-4000-8000-000000000001"
	other := clusterstate.Member{ID: "00000000-0000-4000-8000-000000000002", Name: "other"}
	s.noteClocks(map[string]clockSample{other.ID: {ok: true, offset: 6 * time.Second, uncertainty: 100 * time.Millisecond}})
	if p, crit := s.clockProblem([]clusterstate.Member{other}); !strings.Contains(p, "other's clock is +6.000 s") || crit {
		t.Fatalf("%q %v", p, crit)
	}
	// An attempt without a sample keeps the last sample and says why.
	s.noteClocks(map[string]clockSample{other.ID: {unknown: "round trip 3s, over 2s"}})
	st := s.clockStatus([]clusterstate.Member{other})
	if len(st) != 1 || !st[0].Known || !strings.Contains(st[0].Note, "over 2s") {
		t.Fatalf("%+v", st)
	}
}
