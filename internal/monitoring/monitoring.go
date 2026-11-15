// Package monitoring samples the node's current resource use (ADR 0010
// decision 15): the space of every allow-listed pool, the disk use of
// every VM, and the CPU time and resident memory of each running VM's
// bhyve process. It keeps the latest sample only; there is no history.
// The sample also feeds two health checks, storage_capacity and
// storage_locked.
//
// Sampling only reads: zpool and zfs listings, statfs of directory pools,
// and ps(1) of bhyve processes, all through internal/hostops.
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// DefaultInterval is how often the agent samples. The VM watcher runs
// every two seconds; a zfs listing of every pool that often would cost
// more than the values are worth.
const DefaultInterval = 30 * time.Second

// Default thresholds of the storage_capacity check.
const (
	DefaultWarnPercent = 80
	DefaultFailPercent = 90
)

// VMLister pages through the VMs the agent knows.
type VMLister interface {
	VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error)
}

// Options configures a Sampler.
type Options struct {
	NodeID string
	Host   hostops.Ops
	VMs    VMLister
	// WarnPercent and FailPercent are storage.capacity_warn_percent and
	// storage.capacity_fail_percent; zero means the default.
	WarnPercent int
	FailPercent int
	// MaxAge is how old a sample may be before Metrics takes a new one.
	// Zero means one minute.
	MaxAge time.Duration
	Logger *slog.Logger
	Now    func() time.Time
}

// Sampler takes and keeps the latest sample.
type Sampler struct {
	nodeID     string
	host       hostops.Ops
	vms        VMLister
	warn, fail int
	maxAge     time.Duration
	log        *slog.Logger
	now        func() time.Time

	// sampling serialises samples; mu guards last.
	sampling sync.Mutex
	mu       sync.Mutex
	last     *models.Metrics
}

// New returns a Sampler.
func New(opts Options) (*Sampler, error) {
	if opts.NodeID == "" || opts.VMs == nil || opts.Host.ZFS == nil || opts.Host.Procs == nil {
		return nil, errors.New("monitoring: a node ID, a VM lister, and the host operations are required")
	}
	if opts.WarnPercent == 0 {
		opts.WarnPercent = DefaultWarnPercent
	}
	if opts.FailPercent == 0 {
		opts.FailPercent = DefaultFailPercent
	}
	if opts.WarnPercent < 1 || opts.FailPercent > 100 || opts.WarnPercent >= opts.FailPercent {
		return nil, fmt.Errorf("monitoring: thresholds must satisfy 1 <= warn (%d) < fail (%d) <= 100", opts.WarnPercent, opts.FailPercent)
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sampler{
		nodeID: opts.NodeID, host: opts.Host, vms: opts.VMs, warn: opts.WarnPercent, fail: opts.FailPercent,
		maxAge: opts.MaxAge, log: opts.Logger, now: opts.Now,
	}, nil
}

// Run samples every interval until ctx ends. The first sample is taken at
// once.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.Sample(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("monitoring: sampling failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Metrics returns the latest sample, taking a new one when there is none
// or it is older than MaxAge.
func (s *Sampler) Metrics(ctx context.Context) (models.Metrics, error) {
	if m, ok := s.latest(); ok && s.now().Sub(m.ObservedAt) < s.maxAge {
		return m, nil
	}
	return s.Sample(ctx)
}

// latest returns a copy of the last sample.
func (s *Sampler) latest() (models.Metrics, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return models.Metrics{}, false
	}
	return *s.last, true
}

// Sample takes a new sample and keeps it. A pool or a VM whose values
// cannot be read is reported with an error in the sample; only a failure
// to list the pools or the VMs fails the whole sample.
func (s *Sampler) Sample(ctx context.Context) (models.Metrics, error) {
	s.sampling.Lock()
	defer s.sampling.Unlock()

	m := models.Metrics{
		NodeID: s.nodeID, ObservedAt: s.now().UTC(),
		CapacityWarnPercent: s.warn, CapacityFailPercent: s.fail,
		Pools: []models.PoolMetrics{}, VMs: []models.VMMetrics{},
	}
	pools, err := s.host.Disks().Pools(ctx)
	if err != nil {
		return models.Metrics{}, fmt.Errorf("monitoring: list pools: %w", err)
	}
	// The ZFS datasets under each readable pool's root, by name: disk
	// use and key status.
	inventory := map[string][]hostops.Dataset{}
	for _, p := range pools {
		pm := s.poolMetrics(p)
		if p.Error == "" && p.Kind == hostops.PoolKindZFS {
			list, err := s.host.ZFS.Inventory(ctx, p.Name)
			if err != nil {
				s.log.Warn("monitoring: could not list a pool's datasets", "pool", p.Name, "error", err)
				pm.Error = "its datasets could not be listed"
			} else {
				inventory[p.Name] = list
				for _, d := range list {
					if !d.IsSnapshot() && d.Locked() {
						pm.LockedDatasets = append(pm.LockedDatasets, d.Name)
					}
				}
			}
		}
		m.Pools = append(m.Pools, pm)
	}

	vms, err := s.allVMs(ctx)
	if err != nil {
		return models.Metrics{}, fmt.Errorf("monitoring: list VMs: %w", err)
	}
	for _, vm := range vms {
		m.VMs = append(m.VMs, s.vmMetrics(ctx, vm, inventory))
	}

	s.mu.Lock()
	s.last = &m
	s.mu.Unlock()
	return m, nil
}

func (s *Sampler) allVMs(ctx context.Context) ([]models.VM, error) {
	var out []models.VM
	opts := models.VMListOptions{Limit: 200}
	for {
		page, err := s.vms.VMs(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		opts.Cursor = page.NextCursor
	}
}

// poolMetrics computes a pool's capacity and its state against the
// thresholds.
func (s *Sampler) poolMetrics(p hostops.Pool) models.PoolMetrics {
	pm := models.PoolMetrics{
		Name: p.Name, Type: p.Kind, Health: p.Health, Error: p.Error,
		SizeBytes: p.SizeBytes, AllocatedBytes: p.AllocatedBytes, AvailableBytes: p.AvailableBytes,
	}
	if p.Error != "" {
		pm.Health, pm.Capacity = "UNAVAILABLE", models.CapacityUnavailable
		return pm
	}
	pm.CapacityPercent = CapacityPercent(p)
	switch {
	case pm.CapacityPercent >= float64(s.fail):
		pm.Capacity = models.CapacityFailing
	case pm.CapacityPercent >= float64(s.warn):
		pm.Capacity = models.CapacityWarning
	default:
		pm.Capacity = models.CapacityOK
	}
	return pm
}

// CapacityPercent is the share of a pool in use, rounded to one decimal:
// for a ZFS pool allocated of size, as zpool list reports it; for a
// directory pool used of used plus available, as df(1) reports it.
func CapacityPercent(p hostops.Pool) float64 {
	used, total := p.AllocatedBytes, p.SizeBytes
	if p.Kind == hostops.PoolKindDirectory {
		total = p.AllocatedBytes + p.AvailableBytes
	}
	if total <= 0 || used <= 0 {
		return 0
	}
	pct := float64(used) * 100 / float64(total)
	return float64(int64(pct*10+0.5)) / 10
}

// vmMetrics reads one VM's process and disk use.
func (s *Sampler) vmMetrics(ctx context.Context, vm models.VM, inventory map[string][]hostops.Dataset) models.VMMetrics {
	vmm := models.VMMetrics{ID: vm.ID, Name: vm.Name, State: vm.Observed.State, Disks: []models.DiskMetrics{}}
	var problems []string
	if vm.Observed.State == models.VMRunning && vm.Observed.PID != nil {
		u, err := s.host.Procs.BhyveUsage(ctx, vm.ID, *vm.Observed.PID)
		switch {
		case err == nil:
			cpu := u.CPUTime.Seconds()
			rss := u.ResidentBytes
			vmm.CPUTimeSeconds, vmm.ResidentMemoryBytes = &cpu, &rss
		case errors.Is(err, models.ErrNotFound):
			// It stopped since the watcher looked; the next sample
			// sees it stopped.
		default:
			s.log.Warn("monitoring: could not read a VM's process", "vm_id", vm.ID, "error", err)
			problems = append(problems, "its bhyve process could not be read")
		}
	}

	// The pools of the VM's disks, and its datasets or files there.
	seen := map[string]bool{}
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD || d.Pool == "" || seen[d.Pool] {
			continue
		}
		seen[d.Pool] = true
		used, err := s.diskUse(ctx, vm, d.Pool, inventory, &vmm)
		if err != nil {
			s.log.Warn("monitoring: could not read a VM's disks", "vm_id", vm.ID, "pool", d.Pool, "error", err)
			problems = append(problems, "its disks in "+d.Pool+" could not be read")
			continue
		}
		vmm.DiskUsedBytes += used
	}
	sort.Slice(vmm.Disks, func(i, j int) bool { return vmm.Disks[i].Slot < vmm.Disks[j].Slot })
	vmm.Error = strings.Join(problems, "; ")
	return vmm
}

// diskUse adds the disks of a VM in one pool to vmm and returns the space
// the VM holds there.
func (s *Sampler) diskUse(ctx context.Context, vm models.VM, pool string, inventory map[string][]hostops.Dataset, vmm *models.VMMetrics) (int64, error) {
	kind, err := s.host.Conf.PoolKind(pool)
	if err != nil {
		return 0, err
	}
	slotOf := map[string]int{}
	for _, d := range vm.Disks {
		if d.Pool == pool && d.Bus != models.BusAHCICD {
			slotOf[d.ID] = d.Slot
		}
	}
	if kind == hostops.PoolKindDirectory {
		files, err := s.host.Disks().VMVolumes(ctx, pool, vm.ID)
		if err != nil {
			return 0, err
		}
		var used int64
		for _, f := range files {
			if f.Type != hostops.TypeFile {
				continue
			}
			used += f.UsedBytes
			for _, d := range vm.Disks {
				if d.Pool == pool && path.Base(f.Name) == fmt.Sprintf("disk%d.img", d.Slot) {
					vmm.Disks = append(vmm.Disks, models.DiskMetrics{Slot: d.Slot, Pool: pool, UsedBytes: f.UsedBytes, ReferencedBytes: f.UsedBytes})
					vmm.DiskReferencedBytes += f.UsedBytes
				}
			}
		}
		return used, nil
	}

	list, ok := inventory[pool]
	if !ok {
		return 0, errors.New("the pool's datasets are not available")
	}
	top, err := s.host.Conf.OwnerDataset(pool, hostops.Owner{Kind: hostops.OwnerVM, ID: vm.ID})
	if err != nil {
		return 0, err
	}
	var used int64
	for _, ds := range list {
		if ds.IsSnapshot() || ds.VMID != vm.ID {
			continue
		}
		if ds.Name == top {
			// The VM's dataset holds its disks and their snapshots.
			used = ds.UsedBytes
			continue
		}
		if slot, ok := slotOf[ds.DiskID]; ok && strings.HasPrefix(ds.Name, top+"/") {
			vmm.Disks = append(vmm.Disks, models.DiskMetrics{Slot: slot, Pool: pool, UsedBytes: ds.UsedBytes, ReferencedBytes: ds.ReferencedBytes})
			vmm.DiskReferencedBytes += ds.ReferencedBytes
		}
	}
	return used, nil
}

// CapacityProbe is the storage_capacity health check: it reads the latest
// sample, taking one if there is none or it is stale.
func (s *Sampler) CapacityProbe(ctx context.Context) string {
	m, err := s.Metrics(ctx)
	if err != nil {
		s.log.Warn("monitoring: storage capacity check could not sample", "error", err)
		return "the storage pools could not be read"
	}
	var problems []string
	for _, p := range m.Pools {
		switch p.Capacity {
		case models.CapacityUnavailable:
			problems = append(problems, fmt.Sprintf("pool %s cannot be read", p.Name))
		case models.CapacityFailing:
			problems = append(problems, fmt.Sprintf("pool %s is %.1f%% full, past the %d%% failure threshold", p.Name, p.CapacityPercent, m.CapacityFailPercent))
		case models.CapacityWarning:
			problems = append(problems, fmt.Sprintf("pool %s is %.1f%% full, past the %d%% warning threshold", p.Name, p.CapacityPercent, m.CapacityWarnPercent))
		}
	}
	return strings.Join(problems, "; ")
}

// LockedProbe is the storage_locked health check.
func (s *Sampler) LockedProbe(ctx context.Context) string {
	m, err := s.Metrics(ctx)
	if err != nil {
		return "the storage pools could not be read"
	}
	var locked []string
	for _, p := range m.Pools {
		locked = append(locked, p.LockedDatasets...)
	}
	if len(locked) == 0 {
		return ""
	}
	return "the encryption key is not loaded for " + strings.Join(locked, ", ")
}
