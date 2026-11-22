package containers

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// heldJails holds one container's StartJail until released, so a test can
// act while that jail is recorded starting but not yet running on the host.
type heldJails struct {
	hostops.Jails
	mu      sync.Mutex
	hold    string
	entered chan struct{}
	release chan struct{}
}

func (h *heldJails) StartJail(ctx context.Context, spec hostops.JailSpec) (hostops.JailState, error) {
	h.mu.Lock()
	held := spec.ContainerID == h.hold
	h.mu.Unlock()
	if held {
		close(h.entered)
		<-h.release
	}
	return h.Jails.StartJail(ctx, spec)
}

func (h *heldJails) holdNext(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold, h.entered, h.release = id, make(chan struct{}), make(chan struct{})
}

// TestStartAddressRace is SR-07: two starts of two jails with the same
// static address on one bridge. The host check at submission sees neither
// jail running, so before the store refused it inside MarkStarting's
// transaction both jails ended up running with the same address.
func TestStartAddressRace(t *testing.T) {
	var held *heldJails
	f := newFixture(t, func(o *Options) {
		held = &heldJails{Jails: o.Host.Jails}
		o.Host.Jails = held
	})
	ctx := context.Background()
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/25", "")}}})
	other := f.create(models.ContainerCreate{Name: "other", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge1", IPv4: static("192.0.2.10/24", "")}}})

	held.holdNext(a.ID)
	startA, err := f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	<-held.entered // a is recorded starting; jail(8) has not made it run yet

	// b passes the host check at submission (a is not running yet)...
	startB, err := f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub)
	if err != nil {
		close(held.release)
		t.Fatalf("b's start was refused at submission, so the race was not reached: %v", err)
	}
	doneB := f.wait(startB.ID)
	// ...and is refused when its start is recorded.
	close(held.release)
	f.succeed(startA, nil)
	if doneB.Status != models.TaskFailed || doneB.ErrorCode != models.CodeContainerAddressInUse {
		t.Fatalf("b's start while a was starting with the same address: %s %s %s",
			doneB.Status, doneB.ErrorCode, doneB.ErrorMessage)
	}
	if n := f.count(`SELECT COUNT(*) FROM containers WHERE observed_state = 'running'`); n != 1 {
		t.Fatalf("%d jails running, want 1 (only a)", n)
	}
	got, err := f.svc.Container(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerStopped || got.StartedOnce {
		t.Fatalf("b after the refusal: state %s, started_once %v; want stopped and never started", got.State, got.StartedOnce)
	}

	// Another bridge is another network, while a starts or runs.
	held.holdNext(other.ID)
	startOther, err := f.svc.Action(ctx, other.ID, models.ContainerAction{Action: "start"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	<-held.entered
	close(held.release)
	f.succeed(startOther, nil)

	// Once a has stopped, b starts.
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.succeed(f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub))
}

// TestStartAddressPeerRemovedOutsideSkep: a jail recorded running but
// removed outside Skep does not hold its address at MarkStarting; the
// start compares it with the host first.
func TestStartAddressPeerRemovedOutsideSkep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.SetJailStopped(a.ID) // removed outside Skep; still recorded running
	if n := f.count(`SELECT COUNT(*) FROM containers WHERE id = ? AND observed_state = 'running'`, a.ID); n != 1 {
		t.Fatal("a is not recorded running, so this test does not reach the case")
	}
	f.succeed(f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub))
	if n := f.count(`SELECT COUNT(*) FROM containers WHERE id = ? AND observed_state = 'stopped'`, a.ID); n != 1 {
		t.Fatal("a is not recorded stopped after b's start compared it with the host")
	}
}

// lockProbeJails takes and releases the database's write lock from inside
// every RunningJails call. If the host were observed while MarkStarting's
// transaction (BEGIN IMMEDIATE) held that lock, the probe would wait for
// it; the host is observed first, so the probe never waits.
type lockProbeJails struct {
	hostops.Jails
	db    *sql.DB
	calls atomic.Int32
	err   atomic.Value
}

func (l *lockProbeJails) RunningJails(ctx context.Context) (map[string]hostops.JailState, error) {
	l.calls.Add(1)
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	began := time.Now()
	tx, err := l.db.BeginTx(pctx, nil) // _txlock immediate: takes the write lock
	if err == nil {
		err = tx.Commit()
	}
	if err == nil && time.Since(began) > time.Second {
		err = fmt.Errorf("waited %s for the write lock", time.Since(began))
	}
	if err != nil {
		l.err.CompareAndSwap(nil, err)
	}
	return l.Jails.RunningJails(ctx)
}

// TestStartAddressObservesHostOutsideTransaction: the host re-observation
// of a peer recorded running happens before MarkStarting's write
// transaction, never inside it (#44 review): host commands must not run
// while every other writer is blocked.
func TestStartAddressObservesHostOutsideTransaction(t *testing.T) {
	var probe *lockProbeJails
	f := newFixture(t, func(o *Options) {
		probe = &lockProbeJails{Jails: o.Host.Jails}
		o.Host.Jails = probe
	})
	probe.db = f.db
	ctx := context.Background()
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.SetJailStopped(a.ID) // still recorded running: b's start re-observes it
	before := probe.calls.Load()
	f.succeed(f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub))
	if probe.calls.Load() == before {
		t.Fatal("b's start did not list the running jails, so this test does not reach the case")
	}
	if err, _ := probe.err.Load().(error); err != nil {
		t.Fatalf("the host was observed while the write lock was held: %v", err)
	}
}
