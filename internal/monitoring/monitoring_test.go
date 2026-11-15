package monitoring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	vmA   = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"
	vmB   = "1c1c6f4e-5d0c-4a52-9a43-0d2f8a1f3c22"
	diskA = "7e3c1a90-1111-4222-8333-444455556666"
	diskB = "8e3c1a90-1111-4222-8333-444455556677"
)

type stubZFS struct {
	hostops.ZFS
	pools     []hostops.Pool
	inventory map[string][]hostops.Dataset
	invErr    error
}

func (z *stubZFS) Pools(context.Context) ([]hostops.Pool, error) { return z.pools, nil }
func (z *stubZFS) Inventory(_ context.Context, pool string) ([]hostops.Dataset, error) {
	if z.invErr != nil {
		return nil, z.invErr
	}
	return z.inventory[pool], nil
}

type stubDirs struct {
	hostops.Dirs
	pools []hostops.Pool
	files map[string][]hostops.Dataset
}

func (d *stubDirs) Pools(context.Context) ([]hostops.Pool, error) { return d.pools, nil }
func (d *stubDirs) VMFiles(_ context.Context, _ string, vmID string) ([]hostops.Dataset, error) {
	return d.files[vmID], nil
}

type stubProcs struct {
	hostops.Procs
	usage map[int]hostops.ProcessUsage
	calls int
}

func (p *stubProcs) BhyveUsage(_ context.Context, _ string, pid int) (hostops.ProcessUsage, error) {
	p.calls++
	u, ok := p.usage[pid]
	if !ok {
		return hostops.ProcessUsage{}, models.ErrNotFound
	}
	return u, nil
}

type stubVMs struct{ vms []models.VM }

// VMs pages one VM at a time, so paging is exercised.
func (s stubVMs) VMs(_ context.Context, opts models.VMListOptions) (models.VMList, error) {
	i := 0
	if opts.Cursor != "" {
		i = int(opts.Cursor[0] - '0')
	}
	if i >= len(s.vms) {
		return models.VMList{}, nil
	}
	list := models.VMList{Items: s.vms[i : i+1]}
	if i+1 < len(s.vms) {
		list.NextCursor = string(rune('0' + i + 1))
	}
	return list, nil
}

type fixture struct {
	zfs   *stubZFS
	dirs  *stubDirs
	procs *stubProcs
	vms   stubVMs
	now   time.Time
}

func newFixture() *fixture {
	pid := 4242
	return &fixture{
		now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
		zfs: &stubZFS{
			pools: []hostops.Pool{{Name: "tank", Health: "ONLINE", SizeBytes: 1000, AllocatedBytes: 500, FreeBytes: 500, AvailableBytes: 480}},
			inventory: map[string][]hostops.Dataset{"tank": {
				{Name: "tank/skep", Type: "filesystem", UsedBytes: 900},
				{Name: "tank/skep/vm", Type: "filesystem", UsedBytes: 800},
				{Name: "tank/skep/vm/" + vmA, Type: "filesystem", UsedBytes: 700, VMID: vmA},
				{Name: "tank/skep/vm/" + vmA + "/disk0", Type: "volume", UsedBytes: 600, ReferencedBytes: 400, VMID: vmA, DiskID: diskA},
				{Name: "tank/skep/vm/" + vmA + "/disk0@skep-x", Type: "snapshot", UsedBytes: 200, VMID: vmA},
			}},
		},
		dirs: &stubDirs{
			pools: []hostops.Pool{{Name: "dir", Kind: hostops.PoolKindDirectory, Health: "ONLINE", SizeBytes: 1000, AllocatedBytes: 100, FreeBytes: 900, AvailableBytes: 850}},
			files: map[string][]hostops.Dataset{vmB: {
				{Name: "/d/skep/vm/" + vmB, Type: "directory"},
				{Name: "/d/skep/vm/" + vmB + "/disk0.img", Type: hostops.TypeFile, VolSizeBytes: 1 << 30, UsedBytes: 98304},
			}},
		},
		procs: &stubProcs{usage: map[int]hostops.ProcessUsage{pid: {CPUTime: 2500 * time.Millisecond, ResidentBytes: 64 << 20}}},
		vms: stubVMs{vms: []models.VM{
			{ID: vmA, Name: "alpha", Observed: models.VMObserved{State: models.VMRunning, PID: &pid},
				Disks: []models.VMDisk{{ID: diskA, Slot: 0, Bus: "virtio-blk", Pool: "tank"}, {Slot: 1, Bus: models.BusAHCICD}}},
			{ID: vmB, Name: "beta", Observed: models.VMObserved{State: models.VMStopped},
				Disks: []models.VMDisk{{ID: diskB, Slot: 0, Bus: "virtio-blk", Pool: "dir"}}},
		}},
	}
}

func (f *fixture) sampler(t *testing.T, warn, fail int) *Sampler {
	t.Helper()
	s, err := New(Options{
		NodeID: "node-1",
		Host: hostops.Ops{
			Conf: hostops.Confinement{Pools: []string{"tank"}, DatasetRoot: "skep", Directories: []hostops.DirPool{{Name: "dir", Path: "/d"}}},
			ZFS:  f.zfs, Dirs: f.dirs, Procs: f.procs,
		},
		VMs: f.vms, WarnPercent: warn, FailPercent: fail,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSampleReportsPoolsAndVMs(t *testing.T) {
	f := newFixture()
	m, err := f.sampler(t, 0, 0).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.CapacityWarnPercent != 80 || m.CapacityFailPercent != 90 || m.NodeID != "node-1" || !m.ObservedAt.Equal(f.now) {
		t.Errorf("header = %+v", m)
	}
	if len(m.Pools) != 2 {
		t.Fatalf("pools = %+v", m.Pools)
	}
	if p := m.Pools[0]; p.Name != "tank" || p.Type != "zfs" || p.CapacityPercent != 50 || p.Capacity != models.CapacityOK {
		t.Errorf("tank = %+v", p)
	}
	// df-style: 100 used of 100 + 850.
	if p := m.Pools[1]; p.Name != "dir" || p.Type != "directory" || p.CapacityPercent != 10.5 || p.Capacity != models.CapacityOK {
		t.Errorf("dir = %+v", p)
	}
	if len(m.VMs) != 2 {
		t.Fatalf("vms = %+v", m.VMs)
	}
	a := m.VMs[0]
	if a.CPUTimeSeconds == nil || *a.CPUTimeSeconds != 2.5 || a.ResidentMemoryBytes == nil || *a.ResidentMemoryBytes != 64<<20 {
		t.Errorf("alpha process = %+v", a)
	}
	if a.DiskUsedBytes != 700 || a.DiskReferencedBytes != 400 || len(a.Disks) != 1 || a.Disks[0] != (models.DiskMetrics{Slot: 0, Pool: "tank", UsedBytes: 600, ReferencedBytes: 400}) || a.Error != "" {
		t.Errorf("alpha disks = %+v", a)
	}
	b := m.VMs[1]
	if b.CPUTimeSeconds != nil || b.ResidentMemoryBytes != nil {
		t.Errorf("a stopped VM has process values: %+v", b)
	}
	if b.DiskUsedBytes != 98304 || len(b.Disks) != 1 || b.Disks[0].Pool != "dir" || b.Disks[0].UsedBytes != 98304 {
		t.Errorf("beta disks = %+v", b)
	}
	if f.procs.calls != 1 {
		t.Errorf("BhyveUsage called %d times, want once (running VMs only)", f.procs.calls)
	}
}

func TestARecycledPIDIsNotReported(t *testing.T) {
	f := newFixture()
	f.procs.usage = nil
	m, err := f.sampler(t, 0, 0).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := m.VMs[0]; a.CPUTimeSeconds != nil || a.Error != "" {
		t.Errorf("alpha = %+v", a)
	}
}

func TestCapacityThresholds(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	if got := f.sampler(t, 0, 0).CapacityProbe(ctx); got != "" {
		t.Errorf("at 50%%: %q", got)
	}
	// The check of the ADR: lower the warning threshold below current use.
	got := f.sampler(t, 40, 90).CapacityProbe(ctx)
	if !strings.Contains(got, "pool tank is 50.0% full, past the 40% warning threshold") || strings.Contains(got, "dir") {
		t.Errorf("warn at 40%%: %q", got)
	}
	got = f.sampler(t, 10, 50).CapacityProbe(ctx)
	if !strings.Contains(got, "pool tank is 50.0% full, past the 50% failure threshold") ||
		!strings.Contains(got, "pool dir is 10.5% full, past the 10% warning threshold") {
		t.Errorf("fail at 50%%: %q", got)
	}
	f.zfs.pools[0].Error = "the zfs kernel module is not loaded"
	s := f.sampler(t, 0, 0)
	if got := s.CapacityProbe(ctx); got != "pool tank cannot be read" {
		t.Errorf("unreadable pool: %q", got)
	}
	m, _ := s.Metrics(ctx)
	if m.Pools[0].Capacity != models.CapacityUnavailable || m.Pools[0].Health != "UNAVAILABLE" {
		t.Errorf("unreadable pool = %+v", m.Pools[0])
	}
	if m.VMs[0].Error == "" {
		t.Error("a VM in an unreadable pool has no error")
	}
}

func TestLockedProbe(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	if got := f.sampler(t, 0, 0).LockedProbe(ctx); got != "" {
		t.Errorf("nothing locked: %q", got)
	}
	inv := f.zfs.inventory["tank"]
	inv[2].KeyStatus = "unavailable"
	inv[4].KeyStatus = "unavailable" // a snapshot is not counted twice
	inv[1].KeyStatus = "available"
	got := f.sampler(t, 0, 0).LockedProbe(ctx)
	if got != "the encryption key is not loaded for tank/skep/vm/"+vmA {
		t.Errorf("locked: %q", got)
	}
}

func TestMetricsReusesAFreshSample(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	s := f.sampler(t, 0, 0)
	if _, err := s.Metrics(ctx); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(30 * time.Second)
	if _, err := s.Metrics(ctx); err != nil {
		t.Fatal(err)
	}
	if f.procs.calls != 1 {
		t.Errorf("a fresh sample was not reused: %d samples", f.procs.calls)
	}
	f.now = f.now.Add(31 * time.Second)
	m, _ := s.Metrics(ctx)
	if f.procs.calls != 2 || !m.ObservedAt.Equal(f.now) {
		t.Errorf("a stale sample was reused: %d samples, observed %v", f.procs.calls, m.ObservedAt)
	}
}

func TestInventoryFailureIsReportedNotFatal(t *testing.T) {
	f := newFixture()
	f.zfs.invErr = errors.New("zfs: boom")
	m, err := f.sampler(t, 0, 0).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Pools[0].Error == "" || m.VMs[0].Error == "" || m.VMs[1].Error != "" {
		t.Errorf("pools %+v, vms %+v", m.Pools, m.VMs)
	}
}

func TestThresholdsAreValidated(t *testing.T) {
	f := newFixture()
	for _, c := range [][2]int{{90, 80}, {80, 80}, {80, 101}, {-1, 50}} {
		_, err := New(Options{NodeID: "n", Host: hostops.Ops{ZFS: f.zfs, Procs: f.procs}, VMs: f.vms, WarnPercent: c[0], FailPercent: c[1]})
		if err == nil {
			t.Errorf("thresholds %v accepted", c)
		}
	}
}
