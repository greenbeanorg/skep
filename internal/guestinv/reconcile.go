package guestinv

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Timings of reconciliation (D30).
const (
	// Every is the period of the regular round.
	Every = 60 * time.Second
	// WatchEvery is the period while an accepted create, clone or delete
	// task is unfinished.
	WatchEvery = 5 * time.Second
	// WatchFor bounds how long one task is watched.
	WatchFor = 15 * time.Minute
	// ReleaseAfter is how old a reservation without its guest must be,
	// by this node's clock, before reconciliation releases it.
	ReleaseAfter = 5 * time.Minute
	// MaxWrites bounds the writes of one round.
	MaxWrites = 200
)

// Agent is what reconciliation reads on this node's agent
// (agentclient.Client). It only reads there (D31).
type Agent interface {
	VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error)
	VM(ctx context.Context, id string) (models.VM, error)
	Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error)
	Container(ctx context.Context, id string) (models.Container, error)
	Task(ctx context.Context, id string) (models.TaskDetail, error)
	TaskByKey(ctx context.Context, key string) (models.Task, error)
}

// Writer is the inventory's writes (Inventory).
type Writer interface {
	List(ctx context.Context) (guestapi.GuestsAnswer, error)
	Add(ctx context.Context, id, kind, name, taskID, actor, requestID string) (guestapi.WriteResult, error)
	Remove(ctx context.Context, id, actor, requestID string) (guestapi.WriteResult, error)
	Release(ctx context.Context, kind, name, key, actor, requestID string) (guestapi.WriteResult, error)
}

// ReconcilerOptions configures the reconciler.
type ReconcilerOptions struct {
	Agent     Agent
	Inventory Writer
	// NodeID returns this node's ID (the owner of what it writes).
	NodeID func(ctx context.Context) (string, error)
	// Quorum is the user copy's quorum, the login's (D33).
	Quorum func() bool
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Reconciler is the one goroutine of skep-api that settles the inventory
// (D30): once at start, every minute, and every 5 seconds while a watched
// task is unfinished.
type Reconciler struct {
	o   ReconcilerOptions
	log *slog.Logger

	mu      sync.Mutex
	watched map[string]time.Time // task ID → watch deadline
	wake    chan struct{}
}

// NewReconciler returns the reconciler. Run starts it.
func NewReconciler(o ReconcilerOptions) *Reconciler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Reconciler{o: o, log: o.Logger, watched: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}

// Watch makes rounds every 5 seconds while the task is unfinished, for at
// most 15 minutes. A task ID that is not valid is ignored.
func (r *Reconciler) Watch(taskID string) {
	if r == nil || !clusterstate.ValidNodeID(taskID) {
		return
	}
	r.mu.Lock()
	r.watched[taskID] = r.o.Now().Add(WatchFor)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run makes rounds until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	last := time.Time{}
	for ctx.Err() == nil {
		now := r.o.Now()
		if last.IsZero() || now.Sub(last) >= Every || r.watching() {
			r.Round(ctx)
			last = r.o.Now()
			r.updateWatched(ctx)
		}
		wait := Every
		if r.watching() {
			wait = WatchEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-r.wake:
		}
	}
}

func (r *Reconciler) watching() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watched) > 0
}

// updateWatched drops the tasks that ended (after the round that saw them
// end) or were watched long enough.
func (r *Reconciler) updateWatched(ctx context.Context) {
	r.mu.Lock()
	ids := make(map[string]time.Time, len(r.watched))
	for id, d := range r.watched {
		ids[id] = d
	}
	r.mu.Unlock()
	actx := apiCaller(ctx)
	for id, deadline := range ids {
		done := r.o.Now().After(deadline)
		if !done {
			t, err := r.o.Agent.Task(actx, id)
			done = errors.Is(err, models.ErrNotFound) || (err == nil && t.Status.Terminal())
		}
		if done {
			r.mu.Lock()
			delete(r.watched, id)
			r.mu.Unlock()
		}
	}
}

// apiCaller is the agent caller of reconciliation: skep-api itself.
func apiCaller(ctx context.Context) context.Context {
	return agentproto.WithCaller(ctx, agentproto.Caller{ActorID: agentproto.APIActor, ActorName: agentproto.APIActor})
}

// agentGuest is one guest on this node's agent.
type agentGuest struct{ id, kind, name string }

// listAgent lists every VM and every jail on the agent, all pages.
func (r *Reconciler) listAgent(ctx context.Context) ([]agentGuest, error) {
	var out []agentGuest
	cursor := ""
	for {
		page, err := r.o.Agent.VMs(ctx, models.VMListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, v := range page.Items {
			out = append(out, agentGuest{v.ID, clusterstate.KindVM, v.Name})
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	for {
		page, err := r.o.Agent.Containers(ctx, models.ContainerListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			out = append(out, agentGuest{c.ID, clusterstate.KindContainer, c.Name})
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	return out, nil
}

// RoundResult says what one round did, for tests and the log.
type RoundResult struct {
	// Skipped says why the round wrote nothing before it began, or "".
	Skipped string
	// Stopped says why the round ended early, or "".
	Stopped                 string
	Added, Removed, Release int
}

// Round makes one reconciliation round (D30 a–g). It reads the inventory
// first and only then the agent; any error from the agent ends the round
// with nothing more written. It never submits, repeats or cancels
// anything on the agent (D31).
func (r *Reconciler) Round(ctx context.Context) (res RoundResult) {
	defer func() {
		if res.Added+res.Removed+res.Release > 0 || res.Stopped != "" {
			r.log.Info("guest reconciliation round", "added", res.Added, "removed", res.Removed, "released", res.Release, "stopped", res.Stopped)
		}
	}()
	if r.o.Quorum != nil && !r.o.Quorum() {
		res.Skipped = "no quorum"
		return res
	}
	inv, err := r.o.Inventory.List(ctx)
	switch {
	case err != nil:
		res.Skipped = "the inventory cannot be read: " + err.Error()
		return res
	case inv.ClusterID == "" || inv.Joining || inv.Removed || inv.Stalled:
		res.Skipped = "this node's inventory is not the cluster's (joining, removed, stalled or no cluster)"
		return res
	}
	node, err := r.o.NodeID(ctx)
	if err != nil || node == "" {
		res.Skipped = "this node's ID is not known"
		return res
	}
	actx := apiCaller(ctx)
	onAgent, err := r.listAgent(actx)
	if err != nil {
		res.Stopped = "the agent's lists: " + err.Error()
		return res
	}
	inInv := map[string]guestapi.GuestInfo{}
	for _, g := range inv.Guests {
		inInv[g.ID] = g
	}
	agentIDs, agentNames := map[string]bool{}, map[string]bool{}
	for _, g := range onAgent {
		agentIDs[g.id], agentNames[g.kind+"\x00"+g.name] = true, true
	}
	reserved := map[string]guestapi.ReservationInfo{}
	for _, x := range inv.Reservations {
		if x.Owner == node {
			reserved[x.Kind+"\x00"+x.Name] = x
		}
	}
	writes := 0
	budget := func() bool {
		if writes >= MaxWrites {
			res.Stopped = "the round's limit of writes"
			return false
		}
		writes++
		return true
	}
	// (d) a guest on the agent that the inventory does not have.
	for _, g := range onAgent {
		if _, ok := inInv[g.id]; ok {
			continue
		}
		taskID := ""
		if x, ok := reserved[g.kind+"\x00"+g.name]; ok {
			t, err := r.o.Agent.TaskByKey(actx, x.Key)
			switch {
			case err == nil:
				taskID = t.ID
			case !errors.Is(err, models.ErrNotFound):
				res.Stopped = "the agent's task by key: " + err.Error()
				return res
			}
		}
		if !budget() {
			return res
		}
		_, err := r.o.Inventory.Add(ctx, g.id, g.kind, g.name, taskID, guestapi.ActorReconciliation, "")
		r.log.Info("reconciliation: guest.add", "guest_id", g.id, "kind", g.kind, "name", g.name, "task_id", taskID, "error", errText(err))
		if err == nil {
			res.Added++
		}
	}
	// (e) a guest this node owns that the lists do not have: only the
	// agent's "not found" for that one guest removes it.
	for _, g := range inv.Guests {
		if g.Owner != node || agentIDs[g.ID] {
			continue
		}
		var err error
		if g.Kind == clusterstate.KindVM {
			_, err = r.o.Agent.VM(actx, g.ID)
		} else {
			_, err = r.o.Agent.Container(actx, g.ID)
		}
		if err == nil {
			continue // the list was older than the guest
		}
		if !errors.Is(err, models.ErrNotFound) {
			res.Stopped = "the agent's read of one guest: " + err.Error()
			return res
		}
		if !budget() {
			return res
		}
		_, err = r.o.Inventory.Remove(ctx, g.ID, guestapi.ActorReconciliation, "")
		r.log.Info("reconciliation: guest.remove", "guest_id", g.ID, "kind", g.Kind, "name", g.Name, "error", errText(err))
		if err == nil {
			res.Removed++
		}
	}
	// (f) a reservation of this node with no guest of its kind and name
	// on the agent, older than ReleaseAfter by this node's clock.
	now := r.o.Now()
	for k, x := range reserved {
		if agentNames[k] || now.Sub(time.Unix(x.At, 0)) <= ReleaseAfter {
			continue
		}
		if !budget() {
			return res
		}
		_, err := r.o.Inventory.Release(ctx, x.Kind, x.Name, x.Key, guestapi.ActorReconciliation, "")
		r.log.Info("reconciliation: guest.release", "kind", x.Kind, "name", x.Name, "error", errText(err))
		if err == nil {
			res.Release++
		}
	}
	return res
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
