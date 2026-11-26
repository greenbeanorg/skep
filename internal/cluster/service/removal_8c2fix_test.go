package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/localapi"
)

// handoverPasses records the old leader's pass-ons of its own removal.
type handoverPasses struct {
	mu    sync.Mutex
	codes []string // "" for a pass that succeeded
}

func (p *handoverPasses) add(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes = append(p.codes, opCode(err))
}

func (p *handoverPasses) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.codes...)
}

// removingEntries counts the member.removing entries n applied.
func removingEntries(n *tnode) int {
	return strings.Count(n.logs.String(), "cluster membership: member removing")
}

// F6 of the 8c-2 review: the leader removes itself while the new leader's
// barrier of its term is held. The first pass-on gets "unavailable", a
// repeat succeeds once the barrier is let go, and one entry is written.
func TestRemoveTheLeaderWhileTheNewLeadersBarrierIsHeld(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	old := h.leader()
	release := make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	defer let()
	for _, n := range h.running() {
		if n == old {
			continue
		}
		n.svc.mu.Lock()
		n.svc.beforeBarrier = func() { <-release }
		n.svc.mu.Unlock()
	}
	passes := &handoverPasses{}
	old.svc.mu.Lock()
	old.svc.afterHandoverPass = func(err error) error {
		passes.add(err)
		if err != nil {
			let() // the barrier goes ahead after the first refusal
		}
		return err
	}
	old.svc.mu.Unlock()

	res := h.remove(old, old.name)
	got := passes.list()
	if !res.HandedOver || res.NodeID != old.id || len(got) < 2 || got[0] != "unavailable" || got[len(got)-1] != "" {
		t.Fatalf("result %+v after passes %q", res, got)
	}
	for _, c := range got[1 : len(got)-1] {
		if !handoverRetryable(&localapi.OpError{Code: c}) {
			t.Fatalf("a repeat after %q: %q", c, got)
		}
	}
	h.gone(old, 60*time.Second)
	for _, n := range h.running() {
		if n == old {
			continue
		}
		if c := removingEntries(n); c != 1 {
			t.Fatalf("%s applied %d member.removing entries, want 1", n.name, c)
		}
	}
	if !strings.Contains(old.logs.String(), "passing the removal on again") {
		t.Fatal("the repeat was not logged")
	}
}

// F6 of the 8c-2 review: a pass-on whose outcome is unknown is not
// repeated: the answer says so, and the removal that may have been written
// is not written twice.
func TestRemoveTheLeaderOutcomeUnknownIsNotRepeated(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	old := h.leader()
	passes := &handoverPasses{}
	old.svc.mu.Lock()
	old.svc.afterHandoverPass = func(err error) error {
		if err == nil {
			// The removal was written and its answer is lost.
			err = &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "outcome_unknown", Message: "no answer came"}
		}
		passes.add(err)
		return err
	}
	old.svc.mu.Unlock()

	_, err := old.svc.Remove(context.Background(), localapi.RemoveRequest{Name: old.name}, "root")
	got := passes.list()
	if opCode(err) != "outcome_unknown" || len(got) == 0 || got[len(got)-1] != "outcome_unknown" {
		t.Fatalf("%v after passes %q", err, got)
	}
	for _, c := range got[:len(got)-1] {
		if !handoverRetryable(&localapi.OpError{Code: c}) {
			t.Fatalf("a pass after %q: %q", c, got)
		}
	}
	h.gone(old, 60*time.Second)
	if n := passes.list(); len(n) != len(got) {
		t.Fatalf("passed on again after outcome_unknown: %q", n)
	}
	for _, n := range h.running() {
		if n == old {
			continue
		}
		if c := removingEntries(n); c != 1 {
			t.Fatalf("%s applied %d member.removing entries, want 1", n.name, c)
		}
	}
}

// F7 of the 8c-2 review: a removal's cluster ID compared on a node that
// does not know its cluster ID yet is "not ready" (503 unavailable), not a
// mismatch.
func TestClusterIDOnANodeThatIsNotReady(t *testing.T) {
	err := clusterIDMismatch("00000000-0000-4000-8000-000000000001", "")
	if opCode(err) != "unavailable" || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
	if oe := err.(*localapi.OpError); oe.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("status %d", oe.HTTPStatus)
	}
	if opCode(clusterIDMismatch("00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002")) != CodeClusterIDMismatch {
		t.Fatal("a real mismatch")
	}
	if clusterIDMismatch("", "") != nil || clusterIDMismatch("x", "x") != nil {
		t.Fatal("no ID, or the same ID")
	}
}
