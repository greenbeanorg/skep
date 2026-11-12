package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeDiscoverer struct {
	hw    models.Hardware
	err   error
	calls int
}

func (f *fakeDiscoverer) Discover(context.Context) (models.Hardware, error) {
	f.calls++
	return f.hw, f.err
}

type fakePinger struct{ err error }

func (f fakePinger) PingContext(context.Context) error { return f.err }

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func freebsdHardware() models.Hardware {
	return models.Hardware{
		Platform: models.PlatformFreeBSD,
		Host: models.HostInfo{
			Hostname:     "hearst",
			OS:           models.OSInfo{Name: "FreeBSD", Version: "15.1", Release: "15.1-RELEASE"},
			Architecture: "amd64",
		},
		Hypervisors: models.Hypervisors{
			Bhyve: models.BhyveInfo{Capability: models.Capability{Status: models.StatusAvailable, Reason: "ok"}},
			QEMU:  models.QEMUInfo{Capability: models.Capability{Status: models.StatusUnavailable, Reason: "none"}},
		},
		Storage: models.StorageInfo{
			ZFS: models.ZFSInfo{Capability: models.Capability{Status: models.StatusUnknown, Reason: "?"}},
		},
	}
}

func newTestService(t *testing.T, opts Options) *Service {
	t.Helper()
	if opts.NodeID == "" {
		opts.NodeID = "node-1"
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	s, err := NewService(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewServiceValidation(t *testing.T) {
	if _, err := NewService(Options{Discoverer: &fakeDiscoverer{}}); err == nil {
		t.Error("an empty node ID must be rejected")
	}
	if _, err := NewService(Options{NodeID: "n"}); err == nil {
		t.Error("a missing discoverer must be rejected")
	}
}

func TestSystem(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := newTestService(t, Options{Discoverer: &fakeDiscoverer{hw: freebsdHardware()}, Now: c.now})
	c.advance(90 * time.Second)

	sys, err := s.System(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sys.NodeID != "node-1" || sys.Hostname != "hearst" || sys.OS.Name != "FreeBSD" || sys.Architecture != "amd64" {
		t.Errorf("system = %+v", sys)
	}
	if !sys.ProductionHost || sys.DevMode {
		t.Errorf("FreeBSD outside dev mode must be a production host: %+v", sys)
	}
	if sys.Capabilities.Bhyve.Status != models.StatusAvailable ||
		sys.Capabilities.QEMU.Status != models.StatusUnavailable ||
		sys.Capabilities.ZFS.Status != models.StatusUnknown {
		t.Errorf("capabilities = %+v", sys.Capabilities)
	}
	if sys.UptimeSeconds != 90 || sys.Skep.Version == "" {
		t.Errorf("uptime = %d, version = %q", sys.UptimeSeconds, sys.Skep.Version)
	}
}

func TestSystemIsNeverProductionOnLinuxOrInDevMode(t *testing.T) {
	linux := freebsdHardware()
	linux.Platform = models.PlatformLinuxDev
	for name, opts := range map[string]Options{
		"linux":          {Discoverer: &fakeDiscoverer{hw: linux}},
		"freebsd in dev": {Discoverer: &fakeDiscoverer{hw: freebsdHardware()}, DevMode: true},
		"linux in dev":   {Discoverer: &fakeDiscoverer{hw: linux}, DevMode: true},
	} {
		sys, err := newTestService(t, opts).System(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sys.ProductionHost {
			t.Errorf("%s: reported as a production host", name)
		}
	}
}

func TestHardwareLocalNodeOnly(t *testing.T) {
	s := newTestService(t, Options{Discoverer: &fakeDiscoverer{hw: freebsdHardware()}})

	hw, err := s.Hardware(context.Background(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if hw.NodeID != "node-1" {
		t.Errorf("node_id = %q", hw.NodeID)
	}
	if _, err := s.Hardware(context.Background(), "node-2"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestDiscoveryIsCached(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	d := &fakeDiscoverer{hw: freebsdHardware()}
	s := newTestService(t, Options{Discoverer: d, CacheTTL: 10 * time.Second, Now: c.now})
	ctx := context.Background()

	for range 3 {
		if _, err := s.System(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Hardware(ctx, "node-1"); err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 {
		t.Errorf("discovery ran %d times within the TTL, want 1", d.calls)
	}
	c.advance(11 * time.Second)
	if _, err := s.System(ctx); err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("discovery ran %d times after the TTL, want 2", d.calls)
	}
}

func TestDiscoveryErrorIsNotCached(t *testing.T) {
	d := &fakeDiscoverer{err: context.Canceled}
	s := newTestService(t, Options{Discoverer: d, CacheTTL: time.Minute})
	if _, err := s.System(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	d.err, d.hw = nil, freebsdHardware()
	if _, err := s.System(context.Background()); err != nil {
		t.Fatalf("a failed pass must not be cached: %v", err)
	}
}

func TestHealth(t *testing.T) {
	ctx := context.Background()

	ok := newTestService(t, Options{Discoverer: &fakeDiscoverer{}, Database: fakePinger{}}).Health(ctx)
	if ok.Status != models.HealthOK || ok.Checks["database"].Status != models.HealthOK || ok.Version == "" {
		t.Errorf("health = %+v", ok)
	}

	bad := newTestService(t, Options{
		Discoverer: &fakeDiscoverer{},
		Database:   fakePinger{err: errors.New("disk I/O error at /var/db/skep/agent/agent.db")},
	}).Health(ctx)
	if bad.Status != models.HealthDegraded || bad.Checks["database"].Status != models.HealthDegraded {
		t.Errorf("health = %+v", bad)
	}
	if got := bad.Checks["database"].Error; got != "database is not reachable" {
		t.Errorf("health must not leak internal error text, got %q", got)
	}

	none := newTestService(t, Options{Discoverer: &fakeDiscoverer{}}).Health(ctx)
	if none.Status != models.HealthOK || len(none.Checks) != 0 || none.Checks == nil {
		t.Errorf("health without database = %+v", none)
	}
}

type fakeNodes struct {
	saved  []models.Hardware
	nodeID string
	err    error
}

func (f *fakeNodes) SaveObserved(_ context.Context, nodeID string, hw models.Hardware) error {
	if f.err != nil {
		return f.err
	}
	f.nodeID = nodeID
	f.saved = append(f.saved, hw)
	return nil
}

type fakeReporter struct{ messages []string }

func (f *fakeReporter) Progress(message string, _ any) { f.messages = append(f.messages, message) }

type fakeTasks struct {
	items  []tasks.Task
	events []models.TaskEvent
	opts   models.TaskListOptions
}

func (f *fakeTasks) Get(_ context.Context, id string) (tasks.Task, error) {
	for _, t := range f.items {
		if t.ID == id {
			return t, nil
		}
	}
	return tasks.Task{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
}

func (f *fakeTasks) Events(context.Context, string) ([]models.TaskEvent, error) {
	return f.events, nil
}

func (f *fakeTasks) List(_ context.Context, opts models.TaskListOptions) ([]tasks.Task, string, error) {
	f.opts = opts
	return f.items, "cursor-2", nil
}

func TestRefreshTask(t *testing.T) {
	d := &fakeDiscoverer{hw: freebsdHardware()}
	nodes := &fakeNodes{}
	s := newTestService(t, Options{Discoverer: d, Nodes: nodes, CacheTTL: time.Minute})
	ctx := context.Background()

	def := s.RefreshDefinition()
	if def.Type != TaskDiscoveryRefresh || !def.Cancellable || def.Timeout <= 0 {
		t.Errorf("definition = %+v", def)
	}

	// Prime the cache, then refresh: the task must run a fresh pass even
	// though the cache is still valid.
	if _, err := s.System(ctx); err != nil {
		t.Fatal(err)
	}
	rep := &fakeReporter{}
	out, err := def.Run(ctx, tasks.Task{}, rep)
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("discovery ran %d times, want 2 (the task must not use the cache)", d.calls)
	}
	res, ok := out.(RefreshResult)
	if !ok || res.Platform != models.PlatformFreeBSD || res.Hostname != "hearst" ||
		res.Capabilities.Bhyve.Status != models.StatusAvailable {
		t.Errorf("result = %+v", out)
	}
	if len(nodes.saved) != 1 || nodes.nodeID != "node-1" || nodes.saved[0].NodeID != "node-1" {
		t.Errorf("observed state not stored: %+v", nodes)
	}
	if len(rep.messages) != 2 {
		t.Errorf("progress = %v", rep.messages)
	}
	// The refreshed result is what the API now serves, without another pass.
	if _, err := s.System(ctx); err != nil || d.calls != 2 {
		t.Errorf("calls = %d, err = %v", d.calls, err)
	}
}

func TestRefreshTaskFailures(t *testing.T) {
	ctx := context.Background()

	// A cancelled discovery returns the context error so the engine can
	// tell a cancel from a failure.
	s := newTestService(t, Options{Discoverer: &fakeDiscoverer{err: context.Canceled}})
	if _, err := s.RefreshDefinition().Run(ctx, tasks.Task{}, &fakeReporter{}); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}

	s = newTestService(t, Options{
		Discoverer: &fakeDiscoverer{hw: freebsdHardware()},
		Nodes:      &fakeNodes{err: errors.New("disk full")},
	})
	if _, err := s.RefreshDefinition().Run(ctx, tasks.Task{}, &fakeReporter{}); err == nil {
		t.Error("a failed store must fail the task")
	}
}

func TestTasksAndTask(t *testing.T) {
	ctx := context.Background()
	ft := &fakeTasks{
		items: []tasks.Task{{
			ID: "t1", Type: TaskDiscoveryRefresh, Status: models.TaskFailed,
			Actor:     models.TaskActor{ID: "system", Name: "system"},
			PeerUID:   0,
			ErrorCode: "timeout", ErrorMessage: "the task exceeded its time limit",
			Payload: []byte(`{"internal":true}`), IdempotencyKey: "secret-key",
		}},
		events: []models.TaskEvent{{ID: 1, Kind: models.TaskEventState, Message: "task is pending"}},
	}
	s := newTestService(t, Options{Discoverer: &fakeDiscoverer{}, Tasks: ft})

	list, err := s.Tasks(ctx, models.TaskListOptions{Limit: 5, Status: models.TaskFailed})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.NextCursor != "cursor-2" || ft.opts.Limit != 5 {
		t.Errorf("list = %+v, opts = %+v", list, ft.opts)
	}
	if e := list.Items[0].Error; e == nil || e.Code != "timeout" {
		t.Errorf("task error = %+v", e)
	}
	ft.items[0].Result = []byte(`{"big":"document"}`)
	list, _ = s.Tasks(ctx, models.TaskListOptions{})
	if list.Items[0].Result != nil {
		t.Error("list entries must not carry the task result")
	}
	if d, _ := s.Task(ctx, "t1"); d.Result == nil {
		t.Error("the task detail must carry the result")
	}
	ft.items[0].Result = nil

	detail, err := s.Task(ctx, "t1")
	if err != nil || detail.ID != "t1" || len(detail.Events) != 1 {
		t.Errorf("detail = %+v, err = %v", detail, err)
	}
	if _, err := s.Task(ctx, "nope"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}

	// Without a task engine there are simply no tasks.
	bare := newTestService(t, Options{Discoverer: &fakeDiscoverer{}})
	empty, err := bare.Tasks(ctx, models.TaskListOptions{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Errorf("empty list = %+v, err = %v", empty, err)
	}
	if _, err := bare.Task(ctx, "t1"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
