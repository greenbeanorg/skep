// Package hostops holds every call the agent makes that needs root: ZFS,
// network interfaces, the vmm devices, the files under Skep's state
// directories, and the per-VM runner processes (ADR 0004 rule 6, ADR 0009
// decision 1b).
//
// Rules of the package:
//
//   - Every command is an absolute, fixed binary path with an explicit
//     argument list. There is no shell and no $PATH.
//   - Every argument that comes from a request is a typed value (a size in
//     bytes, a slot number, a resource ID) before it is rendered. Dataset
//     names, device paths, and file paths are derived from IDs and the
//     configuration, never accepted.
//   - The host confinement (Confinement) comes from the agent's root-owned
//     configuration. Anything outside it is refused here, before a command
//     runs, whoever asks.
//
// The real implementation exists on FreeBSD only (New). Its logic lives in
// untagged files behind two small interfaces, Runner and system, so it is
// unit tested on any OS; only the bindings carry build tags. Development
// machines use the in-memory fake in the hostfake package, which never
// ships in a FreeBSD binary.
package hostops

import (
	"context"
	"io"
	"syscall"
	"time"
)

// Ops is the whole privileged surface, grouped by area. The five fields of
// the real implementation are one value; the fake's are another.
type Ops struct {
	Conf  Confinement
	ZFS   ZFS
	Dirs  Dirs
	Net   Net
	VMM   VMM
	Files Files
	Procs Procs
	// Templates builds jail templates (ADR 0010 decision 7).
	Templates Templates
}

// ZFS creates, lists, and destroys the datasets of VMs inside the
// allow-listed pools. Nothing here can name a dataset outside
// <pool>/<dataset root>.
type ZFS interface {
	// Pools reports every allow-listed pool. A pool that cannot be read
	// (not imported, ZFS not loaded) is reported with an Error, not left
	// out. It never loads the zfs kernel module.
	Pools(ctx context.Context) ([]Pool, error)
	// Pool reports one allow-listed pool, or an error when it cannot be
	// used: pool_not_allowed, storage_unavailable.
	Pool(ctx context.Context, name string) (Pool, error)
	// CreateVMDataset creates <pool>/<root>/vm/<vm-id>, and the datasets
	// above it on first use.
	CreateVMDataset(ctx context.Context, pool, vmID string) (Dataset, error)
	// CreateVolume creates one zvol for a disk of a VM.
	CreateVolume(ctx context.Context, spec VolumeSpec) (Dataset, error)
	// VMDatasets lists the dataset of a VM and everything under it. A VM
	// without a dataset yields an empty list.
	VMDatasets(ctx context.Context, pool, vmID string) ([]Dataset, error)
	// DestroyVM destroys the dataset of a VM recursively and returns what
	// existed. It refuses a dataset that does not carry the VM's ID, and
	// a VM with a snapshot a linked clone depends on
	// (snapshot_has_clones).
	DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error)

	// Inventory lists every filesystem, volume, and snapshot under
	// <pool>/<root>, ordered by name (ADR 0010 decision 1).
	Inventory(ctx context.Context, pool string) ([]Dataset, error)
	// Snapshot takes one atomic recursive snapshot of a resource's
	// dataset, <dataset>@skep-<snapshot-id>, carrying skep:snapshot_id,
	// and returns it as one entry per dataset. A locked dataset is
	// storage_locked.
	Snapshot(ctx context.Context, pool string, o Owner, snapshotID string) ([]Dataset, error)
	// Snapshots lists every snapshot of a resource's datasets, oldest
	// first, Skep's or not.
	Snapshots(ctx context.Context, pool string, o Owner) ([]Dataset, error)
	// Rollback rolls every dataset of a resource back to a snapshot and
	// returns the newer snapshots it destroyed. A snapshot that is not
	// the latest is snapshot_not_latest unless destroyNewer is set; a
	// newer snapshot with clones is snapshot_has_clones; a snapshot that
	// lacks a dataset the resource has now is snapshot_incomplete. The
	// caller makes sure the resource is stopped.
	Rollback(ctx context.Context, pool string, o Owner, snapshotID string, destroyNewer bool) ([]Dataset, error)
	// DestroySnapshot destroys a snapshot on every dataset of a resource
	// and returns what it destroyed. A snapshot with clones is
	// snapshot_has_clones.
	DestroySnapshot(ctx context.Context, pool string, o Owner, snapshotID string) ([]Dataset, error)
	// CloneVM makes a linked clone of every dataset of a VM's snapshot
	// under a new VM ID and returns the clone's datasets.
	CloneVM(ctx context.Context, spec CloneSpec) ([]Dataset, error)
	// ResizeVolume grows the zvol of a VM's disk (ADR 0010 decision 4).
	// It never shrinks one. A volume with a reservation keeps one equal
	// to its size; the pool running out of room is insufficient_space.
	ResizeVolume(ctx context.Context, spec VolumeSpec) (Dataset, error)
	// DestroyVolume destroys the zvol of a VM's disk, only if it carries
	// the VM's and the disk's IDs and has no snapshots left.
	DestroyVolume(ctx context.Context, spec VolumeSpec) (Dataset, error)
}

// Pool is one allow-listed pool as observed.
type Pool struct {
	Name string
	// Kind is PoolKindZFS or PoolKindDirectory.
	Kind string
	// Health is what zpool(8) reports; empty when Error is set.
	Health string
	// Error says why the pool cannot be used. Empty when it can.
	Error          string
	SizeBytes      int64
	AllocatedBytes int64
	FreeBytes      int64
	// AvailableBytes is what a new dataset in the pool could use.
	AvailableBytes int64
}

// VolumeSpec describes a zvol to create. Every field is typed; the dataset
// name is derived.
type VolumeSpec struct {
	Pool   string
	VMID   string
	DiskID string
	Slot   int
	// SizeBytes is the volume size: a multiple of BlockSize.
	SizeBytes int64
	// BlockSize is the volblocksize: a power of two from 4K to 128K.
	BlockSize int
	// Thin creates the volume without a reservation.
	Thin bool
}

// Dataset is a filesystem or volume as zfs(8) lists it.
type Dataset struct {
	Name string
	// Type is "filesystem", "volume", or "snapshot"; for a directory
	// pool, "directory" or "file" (TypeFile), with Name the full path.
	Type            string
	VolSizeBytes    int64
	UsedBytes       int64
	ReferencedBytes int64
	// RefReservationBytes is zero for a thin volume.
	RefReservationBytes int64
	VolMode             string
	VolBlockSize        int
	// CreateTxg is the transaction group that created the dataset or
	// snapshot: the order of snapshots.
	CreateTxg int64
	// Origin is the snapshot a clone was made from; Clones, on a
	// snapshot, lists the clones made from it.
	Origin string
	Clones []string
	// Encryption, KeyStatus, Compression, and CompressRatio are what the
	// operator set on <pool>/<root> and Skep inherits (ADR 0010 decision
	// 5). KeyStatus is "available", "unavailable", or empty when the
	// dataset is not encrypted.
	Encryption    string
	KeyStatus     string
	Compression   string
	CompressRatio string
	// The skep: user properties: skep:vm_id, skep:disk_id,
	// skep:container_id, skep:template_id, and, on snapshots,
	// skep:snapshot_id.
	VMID        string
	DiskID      string
	ContainerID string
	TemplateID  string
	SnapshotID  string
}

// IsSnapshot reports whether the dataset is a snapshot.
func (d Dataset) IsSnapshot() bool { return d.Type == "snapshot" }

// Locked reports whether the dataset is encrypted and its key is not
// loaded.
func (d Dataset) Locked() bool { return d.KeyStatus == "unavailable" }

// Net inspects the bridges guests may join and keeps the taps of VMs.
// Skep never creates, destroys, or re-addresses a bridge.
type Net interface {
	// Bridge describes an allow-listed bridge. A bridge that is not on
	// the allow-list is refused with bridge_not_allowed.
	Bridge(ctx context.Context, name string) (Bridge, error)
	// CreateTap creates a tap for one NIC of a VM, describes it as
	// skep:<vm-id>:<slot>, brings it up, and returns its name.
	CreateTap(ctx context.Context, vmID string, slot int) (string, error)
	// AddBridgeMember adds a tap of a VM to an allow-listed bridge that
	// exists and has no management interface among its members.
	AddBridgeMember(ctx context.Context, bridge, tap string) error
	// DestroyTap destroys a tap, only if its description says it belongs
	// to the VM. A tap that does not exist is not an error.
	DestroyTap(ctx context.Context, vmID, tap string) error
	// VMTaps lists the taps whose description says they belong to the VM.
	VMTaps(ctx context.Context, vmID string) ([]string, error)
}

// Bridge is an allow-listed bridge as observed.
type Bridge struct {
	Name    string
	Exists  bool
	Up      bool
	Members []string
}

// VMM reports what bhyve can do on this host.
type VMM interface {
	// Capabilities reads the state of the hypervisor. It loads nothing.
	Capabilities(ctx context.Context) VMMInfo
	// InstanceExists reports whether the vmm instance of a VM exists.
	InstanceExists(vmID string) bool
	// DestroyInstance destroys the vmm instance of a VM. An instance that
	// does not exist is not an error.
	DestroyInstance(ctx context.Context, vmID string) error
	// ForcePoweroff powers the instance of a VM off at once, as pulling
	// the plug would.
	ForcePoweroff(ctx context.Context, vmID string) error
	// StartBhyve starts bhyve for a VM with a configuration file the
	// runner rendered in the VM's state directory, writing its output to
	// the VM's bhyve.log. Only the runner calls it.
	StartBhyve(ctx context.Context, vmID string) (Process, error)
	// OpenConsole opens the agent's side (B) of a VM's nmdm(4) serial
	// console in raw mode. bhyve holds the A side.
	OpenConsole(vmID string) (io.ReadWriteCloser, error)
}

// Process is a child process: bhyve, as the runner sees it.
type Process interface {
	PID() int
	// Wait blocks until the process ends.
	Wait() ExitStatus
	// Signal sends a signal to the process.
	Signal(sig syscall.Signal) error
}

// ExitStatus is how a process ended: an exit code, or a signal.
type ExitStatus struct {
	// Code is the exit status, -1 when the process was killed by a signal
	// or could not be waited for.
	Code int
	// Signal names the signal that killed the process, if one did.
	Signal string
	Err    error
}

// RunnerSignal is what the agent can ask of a VM's runner.
type RunnerSignal int

const (
	// StopGraceful asks the guest to power off (ACPI power button).
	StopGraceful RunnerSignal = iota + 1
	// StopForce powers the guest off at once.
	StopForce
)

// Procs starts and signals the per-VM runner processes (ADR 0009
// decision 7). A runner is identified by its process ID file and checked
// against the process's own argument list before anything is sent to
// it, so a recycled process ID is never signalled.
type Procs interface {
	// StartRunner starts the runner of a VM in its own session, so it
	// survives the agent, and returns its process ID.
	StartRunner(ctx context.Context, vmID string) (int, error)
	// Runner returns the process ID of the VM's live runner.
	Runner(vmID string) (pid int, ok bool)
	// SignalRunner sends a request to the VM's live runner.
	SignalRunner(vmID string, sig RunnerSignal) error
	// BhyveUsage reads the CPU time and resident memory of a VM's bhyve
	// process (ADR 0010 decision 15). The process must be named bhyve and
	// carry the VM's vmm instance name, so a recycled PID is not
	// reported as the VM; otherwise it is ErrNotFound.
	BhyveUsage(ctx context.Context, vmID string, pid int) (ProcessUsage, error)
}

// ProcessUsage is what a process has used so far.
type ProcessUsage struct {
	// CPUTime is the user and system time the process has accumulated.
	CPUTime time.Duration
	// ResidentBytes is its resident set size.
	ResidentBytes int64
}

// VMMInfo is the state of the hypervisor on this host.
type VMMInfo struct {
	// Simulated is true for the development fake.
	Simulated    bool
	VMMLoaded    bool
	BhyvePresent bool
	// MaxCPU is hw.vmm.maxcpu, zero when vmm is not loaded.
	MaxCPU          int
	HostCPUs        int
	HostMemoryBytes int64
	NmdmAvailable   bool
	// FirmwareCode and FirmwareVars say whether the UEFI firmware image
	// and the variable store template exist in the firmware directory.
	FirmwareCode bool
	FirmwareVars bool
}

// Files reads the media directory and keeps Skep's per-VM state
// directories.
type Files interface {
	// Media lists the ISO files in the media directory.
	Media(ctx context.Context) ([]MediaFile, error)
	// ResolveMedia returns the path of an ISO file by name, after
	// finding it in the listing. A name that is not there is
	// media_not_found.
	ResolveMedia(ctx context.Context, name string) (string, error)
	// PrepareVM creates the persistent directory of a VM and copies the
	// UEFI variable store template into it if the VM has none yet.
	PrepareVM(ctx context.Context, vmID string) error
	// RemoveVM removes the persistent and runtime directories of a VM.
	// Missing directories are not an error.
	RemoveVM(ctx context.Context, vmID string) error

	// PrepareRun creates the runtime directory of a VM (root, 0700).
	PrepareRun(ctx context.Context, vmID string) error
	// WriteRunFile replaces one of the fixed files of a VM's runtime
	// directory (RunSpecFile, RunStateFile, RunnerPIDFile) atomically.
	WriteRunFile(vmID, name string, data []byte) error
	// ReadRunFile reads one of them. A missing file is fs.ErrNotExist.
	ReadRunFile(vmID, name string) ([]byte, error)
	// RemoveRunFile removes one of them. A missing file is not an error.
	RemoveRunFile(vmID, name string) error
	// RunVMs lists the IDs of the VMs that have a runtime directory.
	RunVMs() ([]string, error)
	// WriteStateFile replaces one of the fixed files of a VM's persistent
	// directory (BhyveConfigFile) atomically.
	WriteStateFile(vmID, name string, data []byte) error
	// StateFilePath is the path of a fixed file of a VM's persistent
	// directory (BhyveConfigFile, BhyveLogFile, VMVarsFile).
	StateFilePath(vmID, name string) (string, error)
	// LogTail returns the end of the VM's bhyve.log, at most maxBytes.
	LogTail(vmID string, maxBytes int) string

	// SaveVarsSnapshot copies a VM's UEFI variable store to
	// <state>/<vm-id>/snapshots/<snapshot-id>.fd, durably (ADR 0010
	// decision 2b).
	SaveVarsSnapshot(ctx context.Context, vmID, snapshotID string) error
	// RestoreVarsSnapshot replaces a VM's variable store with the copy
	// taken with a snapshot.
	RestoreVarsSnapshot(ctx context.Context, vmID, snapshotID string) error
	// RemoveVarsSnapshot removes the copy taken with a snapshot. A copy
	// that does not exist is not an error.
	RemoveVarsSnapshot(vmID, snapshotID string) error
	// CloneVars creates the persistent directory of a linked clone with
	// the variable store copied from the source's snapshot.
	CloneVars(ctx context.Context, sourceVMID, snapshotID, targetVMID string) error
}

// MediaFile is one ISO file in the media directory.
type MediaFile struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}
