package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
