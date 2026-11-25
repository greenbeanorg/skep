package service

import (
	"sync"
	"testing"
	"time"
)

// F4 of the 8c-1 review (D12): while Run's step is held (as synchronous
// status calls can hold it for seconds), a new leader still makes the
// barrier of its term, so its notLeader() stops answering "unavailable"
// without waiting for the step.
func TestNewLeaderBarrierDoesNotWaitForTheStep(t *testing.T) {
	h := newHarness(t)
	h.cluster("smith", "langrishe", "star")
	old := h.leader()
	release := make(chan struct{})
	var once sync.Once
	// Registered after the harness: runs before it stops the nodes.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var held []*tnode
	for _, n := range h.running() {
		if n == old {
			continue
		}
		n.svc.mu.Lock()
		n.svc.beforeStep = func() { <-release }
		n.svc.mu.Unlock()
		held = append(held, n)
	}
	h.stop(old)
	var leader *tnode
	h.eventually("a new leader whose barrier is done while its step is held", 15*time.Second, func() bool {
		for _, n := range held {
			if n.st.IsLeader() {
				if _, not := n.svc.notLeader(); !not {
					leader = n
					return true
				}
			}
		}
		return false
	})
	select {
	case <-release:
		t.Fatal("the step was released early")
	default:
	}
	leader.svc.mu.Lock()
	bt := leader.svc.barrierTerm
	leader.svc.mu.Unlock()
	if bt != leader.st.Term() {
		t.Fatalf("barrier term %d, term %d", bt, leader.st.Term())
	}
	once.Do(func() { close(release) })
}
