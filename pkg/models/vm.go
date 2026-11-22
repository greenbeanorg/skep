package models

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Hypervisor engines. Only bhyve is implemented (ADR 0009 decision 1).
const (
	EngineBhyve = "bhyve"
	EngineQEMU  = "qemu"
)

// Firmware types. bhyve boots with UEFI only.
const (
	FirmwareUEFI = "uefi"
	FirmwareBIOS = "bios"
)

// VM states. Desired state is "stopped" or "running"; observed state is any
// of them.
const (
	VMUnknown  = "unknown"
	VMStopped  = "stopped"
	VMStarting = "starting"
	VMRunning  = "running"
	VMStopping = "stopping"
	VMFailed   = "failed"
)

// Disk buses. A disk on BusAHCICD is install media; every other bus is a
// zvol-backed disk.
const (
	BusVirtioBlk = "virtio-blk"
	BusNVMe      = "nvme"
	BusAHCIHD    = "ahci-hd"
	BusAHCICD    = "ahci-cd"
)

// NIC models.
const (
	NICVirtioNet = "virtio-net"
	NICE1000     = "e1000"
)

// VM actions of POST /vms/{id}/actions.
const (
	VMActionStart     = "start"
	VMActionStop      = "stop"
	VMActionForceStop = "force_stop"
	VMActionRestart   = "restart"
)

// Task types of the VM lifecycle (ADR 0009 decision 9).
const (
	TaskVMCreate    = "vm.create"
	TaskVMUpdate    = "vm.update"
	TaskVMDelete    = "vm.delete"
	TaskVMStart     = "vm.start"
	TaskVMStop      = "vm.stop"
	TaskVMForceStop = "vm.force_stop"
	TaskVMRestart   = "vm.restart"
)

// VMResource is the resource type of VM tasks and audit entries.
const VMResource = "vm"

// Limits of a VM definition.
const (
	MinVMMemoryBytes = 128 << 20
	MinDiskBytes     = 16 << 20
	// MaxVMDisks bounds disks and install media together; MaxVMNICs
	// bounds network interfaces. Each device takes one PCI slot.
	MaxVMDisks = 8
	MaxVMNICs  = 4
	// DefaultVolBlockSize is the volblocksize of a new disk.
	DefaultVolBlockSize = 16 << 10
	// MaxVMDescription bounds the description, in bytes.
	MaxVMDescription = 1024
	// Stop timeouts, in seconds.
	DefaultStopTimeoutSeconds = 90
	MaxStopTimeoutSeconds     = 300
)

// Error codes of the VM and storage operations.
const (
	CodeEngineUnsupported   = "engine_unsupported"
	CodeFirmwareUnsupported = "firmware_unsupported"
	CodeNotImplemented      = "not_implemented"
	CodePoolNotAllowed      = "pool_not_allowed"
	CodeBridgeNotAllowed    = "bridge_not_allowed"
	CodeInsufficientSpace   = "insufficient_space"
	CodeInsufficientMemory  = "insufficient_memory"
	CodeMediaNotFound       = "media_not_found"
	CodeBhyveUnavailable    = "bhyve_unavailable"
	CodeStorageUnavailable  = "storage_unavailable"
	CodeVMNameTaken         = "vm_name_taken"
	CodeVMNotStopped        = "vm_not_stopped"
	CodeVMNotRunning        = "vm_not_running"
	CodeVMNotProvisioned    = "vm_not_provisioned"
	CodeStopTimeout         = "stop_timeout"
	CodeVMStartFailed       = "vm_start_failed"
	CodeConsoleBusy         = "console_busy"
	// CodeVMMACInUse refuses a VM NIC whose MAC address a Skep jail
	// already holds, at create and clone, the VM side of
	// container_mac_in_use (ADR 0010 amendment u, 25.5 point 3). A MAC
	// another VM holds is still invalid_request.
	CodeVMMACInUse = "vm_mac_in_use"
	// CodeDiskHasSnapshots refuses removing a disk that snapshots contain,
	// unless destroy_snapshots says to destroy them.
	CodeDiskHasSnapshots = "disk_has_snapshots"

	// Storage management (ADR 0010).
	CodeSnapshotLimit             = "snapshot_limit"
	CodeSnapshotHasClones         = "snapshot_has_clones"
	CodeSnapshotNotLatest         = "snapshot_not_latest"
	CodeSnapshotIncomplete        = "snapshot_incomplete"
	CodeStorageLocked             = "storage_locked"
	CodeStorageFeatureUnsupported = "storage_feature_unsupported"
)

// ByteSize is a number of bytes. In JSON it is a number, or a string with
// an optional binary suffix: "512M", "8G", "1T" (ADR 0009 decision 3c). It
// is always written as a number.
type ByteSize int64

// UnmarshalJSON implements json.Unmarshaler.
func (b *ByteSize) UnmarshalJSON(raw []byte) error {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("a size must be a whole number of bytes or a string such as \"8G\"")
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// ParseByteSize parses "1048576", "512M", "8G", or "1T". The suffixes are
// binary: M is 2^20 bytes.
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	shift := 0
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'M', 'm':
			shift = 20
		case 'G', 'g':
			shift = 30
		case 'T', 't':
			shift = 40
		}
		if shift != 0 {
			s = s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > (1<<62)>>shift {
		return 0, fmt.Errorf("a size must be a whole number of bytes or a string such as \"8G\"")
	}
	return ByteSize(v << shift), nil
}

// VMCPUTopology is the CPU topology shown to the guest. The product of the
// three must equal the VM's CPU count.
type VMCPUTopology struct {
	Sockets int `json:"sockets"`
	Cores   int `json:"cores"`
	Threads int `json:"threads"`
}

// VMVNCConfig is the graphical console. It is not implemented: enabling it
// is refused with not_implemented (ADR 0009 decision 8a).
type VMVNCConfig struct {
	Enabled bool `json:"enabled"`
}

// VMConsoleConfig configures a VM's consoles. The serial console always
// exists.
type VMConsoleConfig struct {
	VNC VMVNCConfig `json:"vnc"`
}

// VMConfig is the part of a VM's configuration the database does not
// constrain. It is stored as one JSON document.
type VMConfig struct {
	CPU     *VMCPUTopology  `json:"cpu,omitempty"`
	Console VMConsoleConfig `json:"console"`
}

// VMObserved is what the agent last saw of a VM on the host.
type VMObserved struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// PID is the bhyve process, while the VM runs.
	PID *int       `json:"pid,omitempty"`
	At  *time.Time `json:"at,omitempty"`
}

// VMDisk is a disk or an install medium of a VM.
type VMDisk struct {
	ID   string `json:"id"`
	Slot int    `json:"slot"`
	Bus  string `json:"bus"`
	// ReadOnly is always true for install media.
	ReadOnly bool `json:"read_only"`
	// SizeBytes, Pool, PoolType, and Thin describe a disk on a pool:
	// a zvol in a ZFS pool, a file in a directory pool (ADR 0010
	// decision 6). They are zero for install media.
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Pool      string `json:"pool,omitempty"`
	// PoolType is "zfs" or "directory".
	PoolType string `json:"pool_type,omitempty"`
	Thin     bool   `json:"thin,omitempty"`
	// VolBlockSize is the zvol's block size; zero in a directory pool.
	VolBlockSize int `json:"volblocksize,omitempty"`
	// Dataset is the zvol, derived from the pool, the VM, and the slot.
	Dataset string `json:"dataset,omitempty"`
	// File is the disk's file in a directory pool, relative to the
	// pool's directory: <root>/vm/<vm-id>/disk<slot>.img.
	File string `json:"file,omitempty"`
	// Media is the name of the ISO file, for install media.
	Media string `json:"media,omitempty"`
}

// VMNIC is a network interface of a VM.
type VMNIC struct {
	ID         string `json:"id"`
	Slot       int    `json:"slot"`
	Model      string `json:"model"`
	MACAddress string `json:"mac_address"`
	Bridge     string `json:"bridge"`
	// Tap is the host's tap interface, while the VM runs.
	Tap string `json:"tap,omitempty"`
}

// VM is a virtual machine: what was asked for, and what the agent last saw.
type VM struct {
	ID          string   `json:"id"`
	NodeID      string   `json:"node_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Engine      string   `json:"engine"`
	CPUCount    int      `json:"cpu_count"`
	MemoryBytes int64    `json:"memory_bytes"`
	Firmware    string   `json:"firmware"`
	Config      VMConfig `json:"config"`
	// Provisioned is true once vm.create has finished. A VM that is not
	// provisioned can only be deleted.
	Provisioned bool `json:"provisioned"`
	// DesiredState is "running" after a start and "stopped" after a stop.
	// Nothing reconciles it in this release (ADR 0009 decision 2c).
	DesiredState string     `json:"desired_state"`
	Observed     VMObserved `json:"observed"`
	// Drift is true when the VM should be running and is not, or the
	// reverse. It is information; nothing acts on it.
	Drift bool     `json:"drift"`
	Disks []VMDisk `json:"disks"`
	NICs  []VMNIC  `json:"nics"`
	// ClonedFromSnapshotID is the snapshot a linked clone was made from.
	// A clone is the same guest as its source: its hostname, SSH host
	// keys, and machine ID are the source's (ADR 0010 decision 3).
	ClonedFromSnapshotID string    `json:"cloned_from_snapshot_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// VMList is one page of VMs, ordered by name.
type VMList struct {
	Items      []VM   `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// VMListOptions selects a page of VMs.
type VMListOptions struct {
	// State filters by observed state when not empty.
	State  string
	Limit  int
	Cursor string
}

// VMDiskSpec asks for a disk or an install medium. With Bus "ahci-cd" it
// names an ISO file in Media; otherwise it names a Pool and a Size.
type VMDiskSpec struct {
	// Bus defaults to "virtio-blk", or to "ahci-cd" when Media is set.
	Bus   string   `json:"bus,omitempty"`
	Size  ByteSize `json:"size,omitempty"`
	Pool  string   `json:"pool,omitempty"`
	Thin  bool     `json:"thin,omitempty"`
	Media string   `json:"media,omitempty"`
	// VolBlockSize defaults to 16K: a power of two from 4K to 128K.
	VolBlockSize ByteSize `json:"volblocksize,omitempty"`
}

// VMNICSpec asks for a network interface.
type VMNICSpec struct {
	// Model defaults to "virtio-net".
	Model string `json:"model,omitempty"`
	// MACAddress is generated when empty.
	MACAddress string `json:"mac_address,omitempty"`
	Bridge     string `json:"bridge"`
}

// VMCreate is the body of POST /vms.
type VMCreate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Engine defaults to "bhyve", Firmware to "uefi".
	Engine   string       `json:"engine,omitempty"`
	Firmware string       `json:"firmware,omitempty"`
	CPUCount int          `json:"cpu_count"`
	Memory   ByteSize     `json:"memory"`
	Config   *VMConfig    `json:"config,omitempty"`
	Disks    []VMDiskSpec `json:"disks,omitempty"`
	NICs     []VMNICSpec  `json:"nics,omitempty"`
}

// VMUpdate is the body of PUT /vms/{id}. Fields that are left out are not
// changed. Disks and NICs are fixed at creation.
type VMUpdate struct {
	Description *string   `json:"description,omitempty"`
	CPUCount    *int      `json:"cpu_count,omitempty"`
	Memory      *ByteSize `json:"memory,omitempty"`
	Config      *VMConfig `json:"config,omitempty"`
	// Media replaces the attached install media: a list of ISO file
	// names, empty to detach everything.
	Media *[]string `json:"media,omitempty"`

	// AddDisks adds zvol-backed disks, with the rules of a create; each
	// takes the lowest free slot (ADR 0010 decision 4).
	AddDisks []VMDiskSpec `json:"add_disks,omitempty"`
	// ResizeDisks grows disks. A disk never shrinks; the guest grows its
	// own partition and filesystem afterwards.
	ResizeDisks []VMDiskResize `json:"resize_disks,omitempty"`
	// RemoveDisks destroys the disks in these slots and their zvols. It
	// needs vm:delete as well as vm:write.
	RemoveDisks []int `json:"remove_disks,omitempty"`
	// DestroySnapshots allows removing a disk that snapshots contain:
	// every such snapshot is destroyed whole, on every disk of the VM.
	DestroySnapshots bool `json:"destroy_snapshots,omitempty"`
}

// VMDiskResize grows the disk in a slot to a new size.
type VMDiskResize struct {
	Slot int      `json:"slot"`
	Size ByteSize `json:"size"`
}

// RemovesDisks reports whether an update destroys disks, which is what
// needs vm:delete.
func (u VMUpdate) RemovesDisks() bool { return len(u.RemoveDisks) > 0 }

// VMAction is the body of POST /vms/{id}/actions.
type VMAction struct {
	Action string `json:"action"`
	// Timeout is how long a stop or restart waits for the guest to shut
	// down, in seconds. Default 90, at most 300.
	Timeout *int `json:"timeout,omitempty"`
}

// VMCreated is the answer to POST /vms: the VM, not yet provisioned, and
// the task that provisions it.
type VMCreated struct {
	VM   VM   `json:"vm"`
	Task Task `json:"task"`
}

// StoragePool is one allow-listed pool as the agent last observed it.
type StoragePool struct {
	Name string `json:"name"`
	// Type is "zfs" or "directory".
	Type string `json:"type"`
	// Capabilities says what the pool's kind can do: a directory pool
	// has no snapshots, clones, or quotas (ADR 0010 decision 6).
	Capabilities StoragePoolCapabilities `json:"capabilities"`
	// Health is the pool's health as zpool(8) reports it, or "UNAVAILABLE"
	// when the pool could not be read; Error says why.
	Health         string `json:"health"`
	Error          string `json:"error,omitempty"`
	SizeBytes      int64  `json:"size_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	FreeBytes      int64  `json:"free_bytes"`
	// AvailableBytes is the space a new dataset could use, which is what
	// a disk's reservation is checked against.
	AvailableBytes int64      `json:"available_bytes"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
}

// StoragePoolCapabilities says which storage features a pool has.
type StoragePoolCapabilities struct {
	Snapshots bool `json:"snapshots"`
	Clones    bool `json:"clones"`
	Quotas    bool `json:"quotas"`
}

// PoolCapabilities returns the capabilities of a kind of pool.
func PoolCapabilities(poolType string) StoragePoolCapabilities {
	if poolType == "zfs" {
		return StoragePoolCapabilities{Snapshots: true, Clones: true, Quotas: true}
	}
	return StoragePoolCapabilities{}
}

// StoragePoolList is the list of allow-listed pools.
type StoragePoolList struct {
	Items []StoragePool `json:"items"`
}

// Media is one ISO file in the media directory. The path is not exposed.
type Media struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// MediaList is the listing of the media directory, ordered by name.
type MediaList struct {
	Items []Media `json:"items"`
}

// EngineCapabilities is what a hypervisor engine can do on this host right
// now. Available is false when anything a VM needs is missing, and Reason
// then says what.
type EngineCapabilities struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	// Simulated is true on a development machine: VMs are simulated in
	// memory and nothing runs. Never true on a FreeBSD host.
	Simulated bool `json:"simulated,omitempty"`
	// VMMLoaded, FirmwareFound, and NmdmAvailable are the three things
	// bhyve needs from the host: the kernel module, the UEFI firmware
	// files, and the console device driver.
	VMMLoaded     bool `json:"vmm_loaded"`
	FirmwareFound bool `json:"firmware_found"`
	NmdmAvailable bool `json:"nmdm_available"`
	// MaxVCPUsPerVM is the largest CPU count a VM may have.
	MaxVCPUsPerVM int `json:"max_vcpus_per_vm"`
	// HostMemoryBytes is physical memory; GuestMemoryBytes is what
	// running VMs may use in total, after the configured reserve.
	HostMemoryBytes  int64 `json:"host_memory_bytes"`
	GuestMemoryBytes int64 `json:"guest_memory_bytes"`
	// Pools and Bridges are the allow-lists of the host confinement.
	Pools   []string `json:"pools"`
	Bridges []string `json:"bridges"`
}
