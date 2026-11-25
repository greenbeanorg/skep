package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// barrierStore is a real one-node store whose barrier fails a given number
// of times; it records the barriers and the writes in order.
type barrierStore struct {
	*store.Node
	mu    sync.Mutex
	fails int
	calls []string
}

func (b *barrierStore) Barrier() error {
	b.mu.Lock()
	b.calls = append(b.calls, "barrier")
	fail := b.fails > 0
	if fail {
		b.fails--
	}
	b.mu.Unlock()
	if fail {
		return errors.New("injected: barrier failed")
	}
	return b.Node.Barrier()
}

func (b *barrierStore) Apply(e clusterstate.Entry) (clusterstate.Result, error) {
	b.mu.Lock()
	b.calls = append(b.calls, e.Type)
	b.mu.Unlock()
	return b.Node.Apply(e)
}

func (b *barrierStore) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// A node counts as having gained leadership only once its barrier has
// succeeded: after a failed barrier, the next round tries the barrier again
// before any leader work, even when the leader interval has passed.
func TestFailedBarrierIsRetriedBeforeLeaderWork(t *testing.T) {
	h := newHarness(t)
	tn := h.node("smith") // a one-node cluster with an address
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
	_, _, gn := ghost(t, "ghost", "127.0.88.9")
	real := time.Now()
	e, _ := clusterstate.NewEntry(clusterstate.TypePendingCreate, "p-1", "root", tn.id, clusterstate.PendingCreate{
		NodeID: gn.NodeID, Name: gn.Name, Address: gn.Address, Pin: gn.Pin, ExpiresAt: real.Add(time.Minute).Unix()})
	if _, err := n.Apply(e); err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	c.advance(time.Hour) // the pending entry is due by the leader's clock
	bs := &barrierStore{Node: n, fails: 2}
	s := New(Options{Config: tn.cfg, Key: tn.key, Now: c.now, LeaderInterval: time.Second})
	s.SetStore(bs, nil)
	ctx := context.Background()

	s.leaderStep(ctx) // gains leadership: barrier fails
	c.advance(2 * time.Second)
	s.leaderStep(ctx) // interval passed: the barrier again, fails again
	if got := bs.log(); len(got) != 2 || got[0] != "barrier" || got[1] != "barrier" {
		t.Fatalf("after two failed barriers: %v, want two barriers and no write", got)
	}
	s.leaderStep(ctx) // the barrier succeeds; then the expiry is proposed
	got := bs.log()
	if len(got) != 4 || got[2] != "barrier" || got[3] != clusterstate.TypePendingExpire {
		t.Fatalf("after the barrier succeeded: %v", got)
	}
	c.advance(2 * time.Second)
	s.leaderStep(ctx) // still the leader: no new barrier
	for _, x := range bs.log()[4:] {
		if x == "barrier" {
			t.Fatalf("a barrier in a later round of the same leadership: %v", bs.log())
		}
	}
}
