package guestinv_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/guestinv"
	"github.com/greenbeanorg/skep/internal/guestinv/guestinvtest"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	vm1  = "d0000000-0000-4000-8000-000000000001"
	ct1  = "d0000000-0000-4000-8000-000000000002"
	ct2  = "d0000000-0000-4000-8000-000000000003"
	tsk1 = "e0000000-0000-4000-8000-000000000001"
)

// fakeAgent is this node's agent as reconciliation reads it. err fails
// every call; listErr only the lists; hideFromList leaves guests out of
// the list (an older list) while a read by ID still finds them.
type fakeAgent struct {
	mu           sync.Mutex
	vms          map[string]models.VM
	cts          map[string]models.Container
	byKey        map[string]models.Task
	err          error
	listErr      error
	hideFromList map[string]bool
	calls        int
}

func newAgent() *fakeAgent {
	return &fakeAgent{vms: map[string]models.VM{}, cts: map[string]models.Container{}, byKey: map[string]models.Task{}, hideFromList: map[string]bool{}}
}

func (a *fakeAgent) call() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.err
}

func (a *fakeAgent) VMs(context.Context, models.VMListOptions) (models.VMList, error) {
	if err := a.call(); err != nil {
		return models.VMList{}, err
	}
	if a.listErr != nil {
		return models.VMList{}, a.listErr
	}
	out := models.VMList{Items: []models.VM{}}
	for _, v := range a.vms {
		if !a.hideFromList[v.ID] {
			out.Items = append(out.Items, v)
		}
	}
	return out, nil
}
func (a *fakeAgent) VM(_ context.Context, id string) (models.VM, error) {
	if err := a.call(); err != nil {
		return models.VM{}, err
	}
	if v, ok := a.vms[id]; ok {
		return v, nil
	}
	return models.VM{}, models.ErrNotFound
}
func (a *fakeAgent) Containers(context.Context, models.ContainerListOptions) (models.ContainerList, error) {
	if err := a.call(); err != nil {
		return models.ContainerList{}, err
	}
	out := models.ContainerList{Items: []models.Container{}}
	for _, c := range a.cts {
		if !a.hideFromList[c.ID] {
			out.Items = append(out.Items, c)
		}
	}
	return out, nil
}
func (a *fakeAgent) Container(_ context.Context, id string) (models.Container, error) {
	if err := a.call(); err != nil {
		return models.Container{}, err
	}
	if c, ok := a.cts[id]; ok {
		return c, nil
	}
	return models.Container{}, models.ErrNotFound
}
func (a *fakeAgent) Task(context.Context, string) (models.TaskDetail, error) {
	return models.TaskDetail{}, models.ErrNotFound
}
func (a *fakeAgent) TaskByKey(_ context.Context, key string) (models.Task, error) {
	if err := a.call(); err != nil {
		return models.Task{}, err
	}
	if t, ok := a.byKey[key]; ok {
		return t, nil
	}
	return models.Task{}, models.ErrNotFound
}

type rig struct {
	cl    *guestinvtest.Cluster
	sock  *guestinvtest.Socket
	agent *fakeAgent
	inv   *guestinv.Inventory
	rec   *guestinv.Reconciler
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{cl: guestinvtest.New(), agent: newAgent(), now: time.Unix(1_900_000_000, 0)}
	r.sock = r.cl.Node(guestinvtest.NodeA)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.inv = guestinv.New(r.sock, quiet)
	r.rec = guestinv.NewReconciler(guestinv.ReconcilerOptions{Agent: r.agent, Inventory: r.inv, Logger: quiet,
		NodeID: func(context.Context) (string, error) { return guestinvtest.NodeA, nil },
		Quorum: func() bool { return true }, Now: func() time.Time { return r.now }})
	return r
}

func (r *rig) guests() clusterstate.Guests { return r.cl.Machine().Guests() }

// D30 (d), (e), (f): a drifted guest is added, a gone guest removed, an
// old reservation without its guest released; a young one stays.
func TestRoundAddsRemovesReleases(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "drift1"}
	if _, err := r.inv.Add(ctx, vm1, clusterstate.KindVM, "gone", "", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.inv.Reserve(ctx, clusterstate.KindVM, "old", "k-old", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * time.Minute)
	r.cl.Now = r.now.Unix()
	if _, err := r.inv.Reserve(ctx, clusterstate.KindVM, "young", "k-young", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	res := r.rec.Round(ctx)
	if res.Added != 1 || res.Removed != 1 || res.Release != 1 || res.Stopped != "" {
		t.Fatalf("round: %+v", res)
	}
	g := r.guests()
	if len(g.Guests) != 1 || g.Guests[0].ID != ct1 || g.Guests[0].TaskID != "" || len(g.Reservations) != 1 || g.Reservations[0].Name != "young" {
		t.Fatalf("after the round: %+v", g)
	}
	// Another node's guest that this agent lacks is never removed.
	other := r.cl.Node(guestinvtest.NodeB)
	if _, err := guestinv.New(other, nil).Add(ctx, ct2, clusterstate.KindContainer, "theirs", "", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	if res := r.rec.Round(ctx); res.Added+res.Removed+res.Release != 0 {
		t.Fatalf("second round: %+v", res)
	}
}

// V3: an agent that is unreachable, answers an error, or is another build
// ends the round with nothing written; so do no quorum and a joining or
// removed node's inventory.
func TestRoundWritesNothingWhenTheAgentFails(t *testing.T) {
	ctx := context.Background()
	for name, err := range map[string]error{
		"unreachable":   agentclient.ErrUnavailable,
		"error":         &models.Error{Code: "internal_error", Message: "boom"},
		"another build": (&agentclient.VersionMismatchError{Agent: "0.1.0 (commit old)", API: "0.1.0 (commit new)"}).Model(),
	} {
		r := newRig(t)
		r.agent.cts[ct1] = models.Container{ID: ct1, Name: "drift1"}
		if _, err := r.inv.Add(ctx, vm1, clusterstate.KindVM, "gone", "", "user:andy", ""); err != nil {
			t.Fatal(err)
		}
		before := r.sock.Writes
		r.agent.err = err
		res := r.rec.Round(ctx)
		if res.Stopped == "" || r.sock.Writes != before {
			t.Errorf("%s: %+v, %d writes", name, res, r.sock.Writes-before)
		}
	}
	// An error on the lists only, after a guest was read: nothing.
	r := newRig(t)
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "drift1"}
	r.agent.listErr = errors.New("list failed")
	if res := r.rec.Round(ctx); res.Stopped == "" || r.sock.Writes != 0 {
		t.Errorf("list error: %+v", res)
	}
	// No quorum: not even the agent is asked.
	r = newRig(t)
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "drift1"}
	noQuorum := guestinv.NewReconciler(guestinv.ReconcilerOptions{Agent: r.agent, Inventory: r.inv,
		NodeID: func(context.Context) (string, error) { return guestinvtest.NodeA, nil }, Quorum: func() bool { return false }})
	if res := noQuorum.Round(ctx); res.Skipped == "" || r.agent.calls != 0 || r.sock.Writes != 0 {
		t.Errorf("no quorum: %+v, %d agent calls", res, r.agent.calls)
	}
	// An unreachable inventory: nothing.
	r.sock.Down = true
	if res := r.rec.Round(ctx); res.Skipped == "" || r.agent.calls != 0 {
		t.Errorf("inventory down: %+v", res)
	}
}

// V4: a guest that the agent's list does not show (the list is older than
// the guest, or paging missed it) is read by its ID; found, it is not
// removed. Only "not found" removes.
func TestRoundDoesNotRemoveAGuestTheListMissed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "web1"}
	if _, err := r.inv.Add(ctx, ct1, clusterstate.KindContainer, "web1", "", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	r.agent.hideFromList[ct1] = true
	if res := r.rec.Round(ctx); res.Removed != 0 || len(r.guests().Guests) != 1 {
		t.Fatalf("a guest the list missed was removed: %+v", res)
	}
	// A read that fails (not "not found") ends the round, nothing removed.
	delete(r.agent.cts, ct1)
	r.agent.err = nil
	failing := &failOnRead{fakeAgent: r.agent}
	rec := guestinv.NewReconciler(guestinv.ReconcilerOptions{Agent: failing, Inventory: r.inv, Quorum: func() bool { return true },
		NodeID: func(context.Context) (string, error) { return guestinvtest.NodeA, nil }})
	if res := rec.Round(ctx); res.Removed != 0 || res.Stopped == "" || len(r.guests().Guests) != 1 {
		t.Fatalf("a failed read removed: %+v", res)
	}
	// Now the agent says not found: removed.
	if res := r.rec.Round(ctx); res.Removed != 1 || len(r.guests().Guests) != 0 {
		t.Fatalf("not found: %+v", res)
	}
}

type failOnRead struct{ *fakeAgent }

func (f *failOnRead) Container(context.Context, string) (models.Container, error) {
	return models.Container{}, agentclient.ErrUnavailable
}

// D30 (d): a guest whose name this node reserved gets the task of the
// reservation's key; the reservation is consumed.
func TestRoundTakesTheTaskOfTheReservation(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.inv.Reserve(ctx, clusterstate.KindContainer, "web1", "key-1", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "web1"}
	r.agent.byKey["key-1"] = models.Task{ID: tsk1}
	if res := r.rec.Round(ctx); res.Added != 1 {
		t.Fatalf("round: %+v", res)
	}
	g := r.guests()
	if len(g.Reservations) != 0 || g.Guests[0].TaskID != tsk1 || g.Guests[0].Key != "key-1" {
		t.Fatalf("after the round: %+v", g)
	}
}

// D30 (g): at most MaxWrites writes in one round.
func TestRoundWriteLimit(t *testing.T) {
	r := newRig(t)
	for i := 0; i < guestinv.MaxWrites+5; i++ {
		id := "f0000000-0000-4000-8000-" + pad(i)
		r.agent.cts[id] = models.Container{ID: id, Name: "c" + pad(i)}
	}
	res := r.rec.Round(context.Background())
	if res.Added != guestinv.MaxWrites || res.Stopped == "" || r.sock.Writes != guestinv.MaxWrites {
		t.Fatalf("round: %+v, %d writes", res, r.sock.Writes)
	}
}

func pad(i int) string {
	s := "000000000000"
	d := []byte(s)
	for j := len(d) - 1; i > 0; j-- {
		d[j] = byte('0' + i%10)
		i /= 10
	}
	return string(d)
}

// D26: a write that cannot be committed is repeated with its operation ID
// and then reported not written (unreachable) or unknown (no answer).
func TestWriteRepeatsAndReportsTheOutcome(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.sock.Down = true
	_, err := r.inv.Reserve(ctx, clusterstate.KindVM, "x", "k", "user:andy", "")
	var unc *guestinv.Uncommitted
	if !errors.As(err, &unc) || unc.Outcome != guestinv.OutcomeNotWritten {
		t.Fatalf("unreachable: %v", err)
	}
	r.sock.Down, r.sock.Unanswered = false, true
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	_, err = r.inv.Reserve(ctx2, clusterstate.KindVM, "x", "k", "user:andy", "")
	if !errors.As(err, &unc) || unc.Outcome != guestinv.OutcomeUnknown {
		t.Fatalf("no answer: %v", err)
	}
	r.sock.Unanswered = false
	if _, err := r.inv.Reserve(context.Background(), clusterstate.KindVM, "x", "k", "user:andy", ""); err != nil {
		t.Fatal(err)
	}
	_, err = r.inv.Reserve(context.Background(), clusterstate.KindVM, "x", "k2", "user:andy", "")
	var ref *guestinv.Refused
	if !errors.As(err, &ref) || ref.Code != clusterstate.CodeNameTaken {
		t.Fatalf("taken: %v", err)
	}
}

// F1 of the 8d-1 review: a write skep-cluster could not commit ends the
// round; a refusal does not.
func TestRoundEndsAtAnUncommittedWrite(t *testing.T) {
	r := newRig(t)
	r.agent.cts[ct1] = models.Container{ID: ct1, Name: "a1"}
	r.agent.cts[ct2] = models.Container{ID: ct2, Name: "a2"}
	r.sock.WriteErr = guestapi.ErrUnreachable
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := r.rec.Round(ctx)
	if res.Stopped == "" || res.Added != 0 || r.sock.Calls == 0 {
		t.Fatalf("round: %+v, %d calls", res, r.sock.Calls)
	}
	// One write was tried (with its repeats), not the second.
	calls := r.sock.Calls
	r.sock.WriteErr = &guestapi.Error{HTTPStatus: http.StatusConflict, Code: clusterstate.CodeOwnerNotMember, Message: "no"}
	r.sock.Calls = 0
	if res := r.rec.Round(context.Background()); res.Stopped != "" || r.sock.Calls != 2 {
		t.Fatalf("refused round: %+v, %d calls (the uncommitted round made %d)", res, r.sock.Calls, calls)
	}
}

// F4 of the 8d-1 review: an answer of 500 or above that is not a repeated
// code is unknown, not a refusal; below 500 it is a refusal.
func TestWriteAnswerAbove500IsUnknown(t *testing.T) {
	r := newRig(t)
	r.sock.WriteErr = &guestapi.Error{HTTPStatus: http.StatusInternalServerError, Code: "internal_error", Message: "x"}
	_, err := r.inv.Reserve(context.Background(), clusterstate.KindVM, "x", "k", "user:andy", "")
	var unc *guestinv.Uncommitted
	if !errors.As(err, &unc) || unc.Outcome != guestinv.OutcomeUnknown || r.sock.Calls != 1 {
		t.Fatalf("500: %v, %d calls", err, r.sock.Calls)
	}
	r.sock.WriteErr = &guestapi.Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: "x"}
	_, err = r.inv.Reserve(context.Background(), clusterstate.KindVM, "x", "k", "user:andy", "")
	var ref *guestinv.Refused
	if !errors.As(err, &ref) {
		t.Fatalf("400: %v", err)
	}
}

// F2 of the 8d-1 review: after a skipped round the next comes after
// RetryEvery, not Every.
func TestRunRetriesASkippedRoundSoon(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	asked := 0
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := guestinv.NewReconciler(guestinv.ReconcilerOptions{Agent: r.agent, Inventory: r.inv, Logger: quiet,
		NodeID: func(context.Context) (string, error) { return guestinvtest.NodeA, nil },
		Quorum: func() bool { mu.Lock(); defer mu.Unlock(); asked++; return asked > 1 }})
	ctx, cancel := context.WithTimeout(context.Background(), guestinv.RetryEvery+1500*time.Millisecond)
	defer cancel()
	rec.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if asked != 2 || r.agent.calls == 0 {
		t.Fatalf("rounds begun: %d, agent calls %d; want a skipped round and one that ran", asked, r.agent.calls)
	}
}
