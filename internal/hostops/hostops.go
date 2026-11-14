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
	"time"
)

// Ops is the whole privileged surface, grouped by area. The five fields of
// the real implementation are one value; the fake's are another.
type Ops struct {
	Conf  Confinement
	ZFS   ZFS
	Net   Net
	VMM   VMM
	Files Files
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
	// existed. It refuses a dataset that does not carry the VM's ID.
	DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error)
}

// Pool is one allow-listed pool as observed.
type Pool struct {
	Name string
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
	// Type is "filesystem" or "volume".
	Type            string
	VolSizeBytes    int64
	UsedBytes       int64
	ReferencedBytes int64
	// RefReservationBytes is zero for a thin volume.
	RefReservationBytes int64
	VolMode             string
	VolBlockSize        int
	// VMID and DiskID are the user properties skep:vm_id and
	// skep:disk_id.
	VMID   string
	DiskID string
}

// Net inspects the bridges guests may join.
type Net interface {
	// Bridge describes an allow-listed bridge. A bridge that is not on
	// the allow-list is refused with bridge_not_allowed.
	Bridge(ctx context.Context, name string) (Bridge, error)
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
}

// MediaFile is one ISO file in the media directory.
type MediaFile struct {
	Name       string
	SizeBytes  int64
	ModifiedAt time.Time
}
