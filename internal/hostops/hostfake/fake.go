// Package hostfake is the in-memory stand-in for internal/hostops on a
// development machine (ADR 0009 decision 1b). It simulates pools, bridges,
// and VMs, changes nothing on the machine it runs on, and proves nothing
// about FreeBSD. The agent uses it on Linux; unit tests use it everywhere.
// It is never linked into a FreeBSD agent.
package hostfake

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Marker is a string only this package contains. CI checks that the
// FreeBSD agent binary does not.
const Marker = "skep-hostfake: simulated host, not FreeBSD"

// Options configures the fake.
type Options struct {
	// PoolBytes is the size of every simulated pool. Default 64 GiB.
	PoolBytes int64
	// HostCPUs and HostMemoryBytes describe the simulated host. Defaults:
	// 8 CPUs, 16 GiB.
	HostCPUs        int
	HostMemoryBytes int64
	Logger          *slog.Logger
	// Runner is what a started runner runs: in the agent and the tests,
	// the real runner loop of the bhyve package, here against the fake.
	// Without it no VM can be started.
	Runner RunnerFunc
}

// RunnerFunc runs one VM until its run ends, taking requests from
// signals. pid is the runner's simulated process ID.
type RunnerFunc func(ctx context.Context, ops hostops.Ops, vmID string, pid int, signals <-chan hostops.RunnerSignal) error

// Fake implements the hostops interfaces in memory. Every allow-listed
// pool and bridge exists. The exported fields let a test break things.
type Fake struct {
	conf  hostops.Confinement
	files hostops.Files
	opts  Options

	mu sync.Mutex
	// ZFSLoaded, when false, makes every ZFS call fail the way the real
	// one does without the kernel module.
	ZFSLoaded bool
	// Info is what Capabilities reports.
	Info hostops.VMMInfo
	// MissingBridges do not exist although they are allow-listed;
	// BridgeMembers lists the members of a bridge.
	MissingBridges map[string]bool
	BridgeMembers  map[string][]string
	// FailCreateVolume makes the next CreateVolume calls fail.
	FailCreateVolume error
	// UnusableDirs maps a directory pool to why it cannot be used.
	UnusableDirs map[string]string

	// IgnoreACPI lists the VMs whose guest ignores the ACPI power
	// button, as an installer menu does.
	IgnoreACPI map[string]bool
	// NoEpair simulates a host without if_epair; DevfsConflict one whose
	// devfs ruleset 6001 holds someone else's rules.
	NoEpair       bool
	DevfsConflict bool
	// QuarantineStart makes the next starts behave like a /dev guard
	// refusal whose removal failed: the jail is left in place and the
	// start fails with container_quarantined (amendment n, SR-06).
	// FailDiscard makes DiscardJail fail, leaving the jail.
	QuarantineStart bool
	FailDiscard     error
	// FailStop makes StopJail fail with it, leaving the jail running (as
	// hostops does when jail -r and jail -R both fail: amendment u).
	FailStop error
	// PresentMACs simulates MAC addresses already present on a bridge (its
	// own, a member's, or learnt), by bridge (amendment n, SR-03).
	PresentMACs map[string][]string
	// RacctDisabled simulates a host booted without kern.racct.enable;
	// CPUs is hw.ncpu (4 when zero).
	RacctDisabled bool
	CPUs          int
	// JailUsages is what JailUsage reports for a running jail, by
	// container ID.
	JailUsages map[string]hostops.JailUsage
	// rctl holds the RCTL rules on each jail's name, by container ID.
	rctl map[string][]string

	datasets map[string]hostops.Dataset
	// diskFiles and dirs are the simulated disk files and VM directories
	// of the directory pools, by path.
	diskFiles map[string]hostops.Dataset
	dirs      map[string]bool
	// snaps holds the snapshots by full name; txg orders them.
	snaps     map[string]hostops.Dataset
	txg       int64
	instances map[string]bool
	// sshdTemplates are the templates whose packages included
	// FreeBSD-ssh; jails maps a running jail's container ID to its JID;
	// jailFiles holds what ConfigureJail wrote.
	sshdTemplates map[string]bool
	jails         map[string]int
	nextJID       int
	jailFiles     map[string]hostops.JailFiles
	// leftoverDevfs are the jails with a devfs a removal outside Skep
	// left on their <root>/dev (amendment l).
	leftoverDevfs map[string]bool
	calls         []string
	// taps maps a tap to its description and bridge.
	taps    map[string]fakeTap
	nextTap int
	// epairs maps an epair host end to its description and bridge;
	// devfsDHCP is true once the DHCP ruleset was added.
	epairs    map[string]fakeTap
	nextEpair int
	devfsDHCP bool
	nextPID   int
	runners   map[string]*fakeRunner
	guests    map[string]*Guest
	ctx       context.Context
	stop      context.CancelFunc
	wg        sync.WaitGroup
}

type fakeTap struct{ desc, bridge string }

type fakeRunner struct {
	pid     int
	signals chan hostops.RunnerSignal
	kill    context.CancelFunc
}

// New returns a fake host for the given confinement.
func New(conf hostops.Confinement, opts Options) *Fake {
	if opts.PoolBytes <= 0 {
		opts.PoolBytes = 64 << 30
	}
	if opts.HostCPUs <= 0 {
		opts.HostCPUs = 8
	}
	if opts.HostMemoryBytes <= 0 {
		opts.HostMemoryBytes = 16 << 30
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Fake{
		ctx: ctx, stop: stop,
		IgnoreACPI: map[string]bool{},
		taps:       map[string]fakeTap{},
		epairs:     map[string]fakeTap{},
		nextPID:    10000,
		runners:    map[string]*fakeRunner{},
		guests:     map[string]*Guest{},
		conf:       conf,
		files:      hostops.NewFiles(conf, opts.Logger),
		opts:       opts,
		ZFSLoaded:  true,
		Info: hostops.VMMInfo{
			Simulated: true, VMMLoaded: true, BhyvePresent: true, MaxCPU: 256,
			HostCPUs: opts.HostCPUs, HostMemoryBytes: opts.HostMemoryBytes,
			NmdmAvailable: true, FirmwareCode: true, FirmwareVars: true,
		},
		MissingBridges: map[string]bool{},
		BridgeMembers:  map[string][]string{},
		datasets:       map[string]hostops.Dataset{},
		diskFiles:      map[string]hostops.Dataset{},
		dirs:           map[string]bool{},
		UnusableDirs:   map[string]string{},
		snaps:          map[string]hostops.Dataset{},
		instances:      map[string]bool{},
		sshdTemplates:  map[string]bool{},
		jails:          map[string]int{},
		rctl:           map[string][]string{},
		nextJID:        1,
		jailFiles:      map[string]hostops.JailFiles{},
		leftoverDevfs:  map[string]bool{},
	}
}

// Ops returns the fake as the hostops surface.
func (f *Fake) Ops() hostops.Ops {
	return hostops.Ops{Conf: f.conf, ZFS: f, Dirs: fakeDirs{f}, Net: f, VMM: f, Files: fakeFiles{Files: f.files, f: f}, Procs: f,
		Templates: fakeTemplates{f}, Jails: fakeJails{f}}
}

// Calls returns what was called so far, one line per mutating call.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// SetInstance makes the vmm instance of a VM exist, or not.
func (f *Fake) SetInstance(vmID string, exists bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if exists {
		f.instances[vmID] = true
	} else {
		delete(f.instances, vmID)
	}
}

// Datasets returns the names of every simulated dataset, sorted.
func (f *Fake) Datasets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.datasets))
	for name := range f.datasets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Set changes the fake's exported fields under its lock.
func (f *Fake) Set(change func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *Fake) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *Fake) zfsLoaded() error {
	if !f.ZFSLoaded {
		return &models.Error{Code: models.CodeStorageUnavailable,
			Message: "the zfs kernel module is not loaded on this host; an administrator enables ZFS, Skep does not"}
	}
	return nil
}

// pool computes a simulated pool: a full reservation takes its size, a
// thin volume takes nothing.
func (f *Fake) pool(name string) hostops.Pool {
	var used int64
	for dsName, d := range f.datasets {
		if strings.HasPrefix(dsName, name+"/") {
			used += d.RefReservationBytes
		}
	}
	return hostops.Pool{
		Name: name, Health: "ONLINE", SizeBytes: f.opts.PoolBytes,
		AllocatedBytes: used, FreeBytes: f.opts.PoolBytes - used, AvailableBytes: f.opts.PoolBytes - used,
	}
}

// Pools implements hostops.ZFS.
func (f *Fake) Pools(context.Context) ([]hostops.Pool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hostops.Pool, 0, len(f.conf.Pools))
	for _, name := range f.conf.Pools {
		if !f.ZFSLoaded {
			out = append(out, hostops.Pool{Name: name, Error: "the zfs kernel module is not loaded"})
			continue
		}
		out = append(out, f.pool(name))
	}
	return out, nil
}

// Pool implements hostops.ZFS.
func (f *Fake) Pool(_ context.Context, name string) (hostops.Pool, error) {
	if err := f.conf.CheckPool(name); err != nil {
		return hostops.Pool{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return hostops.Pool{}, err
	}
	return f.pool(name), nil
}

// CreateVMDataset implements hostops.ZFS.
func (f *Fake) CreateVMDataset(_ context.Context, pool, vmID string) (hostops.Dataset, error) {
	name, err := f.conf.VMDataset(pool, vmID)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return hostops.Dataset{}, err
	}
	if d, ok := f.datasets[name]; ok {
		return d, nil
	}
	if err := f.lockedAbove(name); err != nil {
		return hostops.Dataset{}, err
	}
	f.txg++
	d := hostops.Dataset{Name: name, Type: "filesystem", VMID: vmID, CreateTxg: f.txg}
	f.datasets[name] = d
	f.record("zfs create %s", name)
	return d, nil
}

// CreateVolume implements hostops.ZFS.
func (f *Fake) CreateVolume(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	name, err := f.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return hostops.Dataset{}, err
	}
	if f.FailCreateVolume != nil {
		return hostops.Dataset{}, f.FailCreateVolume
	}
	parent, _ := f.conf.VMDataset(spec.Pool, spec.VMID)
	if _, ok := f.datasets[parent]; !ok {
		return hostops.Dataset{}, fmt.Errorf("hostfake: parent dataset %s does not exist", parent)
	}
	if _, ok := f.datasets[name]; ok {
		return hostops.Dataset{}, fmt.Errorf("hostfake: dataset %s already exists", name)
	}
	if err := f.lockedAbove(name); err != nil {
		return hostops.Dataset{}, err
	}
	f.txg++
	d := hostops.Dataset{
		Name: name, Type: "volume", VolSizeBytes: spec.SizeBytes, VolMode: "dev",
		VolBlockSize: spec.BlockSize, VMID: spec.VMID, DiskID: spec.DiskID, CreateTxg: f.txg,
	}
	if !spec.Thin {
		if spec.SizeBytes > f.pool(spec.Pool).AvailableBytes {
			return hostops.Dataset{}, &models.Error{Code: models.CodeInsufficientSpace,
				Message: "the pool " + spec.Pool + " does not have room for this disk"}
		}
		d.RefReservationBytes, d.UsedBytes = spec.SizeBytes, spec.SizeBytes
	}
	f.datasets[name] = d
	f.record("zfs create -V %d thin=%v %s", spec.SizeBytes, spec.Thin, name)
	return d, nil
}

func (f *Fake) vmDatasets(name string) []hostops.Dataset {
	var list []hostops.Dataset
	for dsName, d := range f.datasets {
		if dsName == name || strings.HasPrefix(dsName, name+"/") {
			list = append(list, d)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// VMDatasets implements hostops.ZFS.
func (f *Fake) VMDatasets(_ context.Context, pool, vmID string) ([]hostops.Dataset, error) {
	name, err := f.conf.VMDataset(pool, vmID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return nil, err
	}
	return f.vmDatasets(name), nil
}

// DestroyVM implements hostops.ZFS.
func (f *Fake) DestroyVM(_ context.Context, pool, vmID string) ([]hostops.Dataset, error) {
	name, err := f.conf.VMDataset(pool, vmID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return nil, err
	}
	list := f.vmDatasets(name)
	if clones := f.clonesUnder(name); len(clones) > 0 {
		return nil, &models.Error{Code: models.CodeSnapshotHasClones,
			Message: "a linked clone depends on the snapshot; delete the clone first",
			Details: map[string]any{"clones": clones}}
	}
	for _, d := range list {
		delete(f.datasets, d.Name)
		f.dropOrigin(d)
	}
	for snapName := range f.snaps {
		if under(snapName, name) {
			delete(f.snaps, snapName)
		}
	}
	if len(list) > 0 {
		f.record("zfs destroy -r %s", name)
	}
	return list, nil
}

// Bridge implements hostops.Net.
func (f *Fake) Bridge(_ context.Context, name string) (hostops.Bridge, error) {
	if err := f.conf.CheckBridge(name); err != nil {
		return hostops.Bridge{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MissingBridges[name] {
		return hostops.Bridge{Name: name}, nil
	}
	return hostops.Bridge{Name: name, Exists: true, Up: true,
		Members: append([]string{}, f.BridgeMembers[name]...)}, nil
}

// Capabilities implements hostops.VMM.
func (f *Fake) Capabilities(context.Context) hostops.VMMInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Info
}

// InstanceExists implements hostops.VMM.
func (f *Fake) InstanceExists(vmID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[vmID]
}

// fakeFiles is the real file implementation, except that the UEFI variable
// store is an empty file: a development machine has no firmware.
type fakeFiles struct {
	hostops.Files
	f *Fake
}

func (ff fakeFiles) PrepareVM(ctx context.Context, vmID string) error {
	if err := writeEmptyVars(ff.f.conf, vmID); err != nil {
		return err
	}
	return ff.Files.PrepareVM(ctx, vmID)
}
