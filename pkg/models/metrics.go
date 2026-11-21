package models

import "time"

// Resource monitoring (ADR 0010 decision 15): current values only, no
// history.

// Names of the storage health checks.
const (
	// CheckStorageCapacity warns when an allow-listed pool or directory
	// pool passes storage.capacity_warn_percent, fails past
	// storage.capacity_fail_percent, and fails when a pool cannot be
	// read.
	CheckStorageCapacity = "storage_capacity"
	// CheckStorageLocked fails when a dataset Skep uses has its
	// encryption key unloaded.
	CheckStorageLocked = "storage_locked"
	// CheckOrphanedInterfaces warns while an interface with a skep:
	// description (a jail's epair or a VM's tap) exists whose owner is not
	// running (ADR 0010 decision 10).
	CheckOrphanedInterfaces = "orphaned_interfaces"
)

// Capacity states of a pool in the metrics.
const (
	CapacityOK          = "ok"
	CapacityWarning     = "warning"
	CapacityFailing     = "failing"
	CapacityUnavailable = "unavailable"
)

// Metrics is the node's current resource use.
type Metrics struct {
	NodeID string `json:"node_id"`
	// ObservedAt is when the values were sampled.
	ObservedAt time.Time `json:"observed_at"`
	// CapacityWarnPercent and CapacityFailPercent are the thresholds of
	// the storage_capacity check.
	CapacityWarnPercent int           `json:"capacity_warn_percent"`
	CapacityFailPercent int           `json:"capacity_fail_percent"`
	Pools               []PoolMetrics `json:"pools"`
	VMs                 []VMMetrics   `json:"vms"`
	// Containers are the jails, with RACCT figures for the running
	// ones.
	Containers []ContainerMetrics `json:"containers"`
}

// ContainerMetrics is one jail's current use. The usage fields come from
// RACCT (rctl -u) and are present only while the jail runs on a host with
// RACCT enabled; the disk fields come from ZFS and are always present.
type ContainerMetrics struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	// CPUPercent is RACCT's pcpu, a recent average where 100 is one busy
	// CPU; approximate, like the pcpu limit.
	CPUPercent *int `json:"cpu_percent,omitempty"`
	// CPUTimeSeconds is the CPU time of the jail's processes, in whole
	// seconds.
	CPUTimeSeconds *float64 `json:"cpu_time_seconds,omitempty"`
	// ResidentMemoryBytes is RACCT's memoryuse.
	ResidentMemoryBytes *int64 `json:"resident_memory_bytes,omitempty"`
	// Processes is the number of processes in the jail.
	Processes *int `json:"processes,omitempty"`
	// OpenFileSlots is RACCT's openfiles: the descriptor-table slots of
	// the jail's processes, the figure the open_files limit applies to.
	// It is not the number of open files.
	OpenFileSlots *int `json:"open_file_slots,omitempty"`
	// DiskUsedBytes is the space the jail root holds, its snapshots
	// included; DiskReferencedBytes is what the root refers to, the
	// blocks it shares with its template included (what disk_quota
	// applies to).
	DiskUsedBytes       int64 `json:"disk_used_bytes"`
	DiskReferencedBytes int64 `json:"disk_referenced_bytes"`
	// Error says what could not be read.
	Error string `json:"error,omitempty"`
}

// PoolMetrics is the space of one allow-listed pool.
type PoolMetrics struct {
	Name string `json:"name"`
	// Type is "zfs" or "directory".
	Type string `json:"type"`
	// Health is what zpool(8) reports ("ONLINE" for a usable directory
	// pool), or "UNAVAILABLE" with Error.
	Health         string `json:"health"`
	Error          string `json:"error,omitempty"`
	SizeBytes      int64  `json:"size_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	AvailableBytes int64  `json:"available_bytes"`
	// CapacityPercent is the share in use: allocated of size for a ZFS
	// pool, as zpool list shows it; used of used plus available for a
	// directory pool, as df(1) shows it (the root reserve is not
	// available to Skep's files either).
	CapacityPercent float64 `json:"capacity_percent"`
	// Capacity is CapacityOK, CapacityWarning, CapacityFailing, or
	// CapacityUnavailable.
	Capacity string `json:"capacity"`
	// LockedDatasets lists the datasets under the pool's dataset root
	// whose encryption key is not loaded.
	LockedDatasets []string `json:"locked_datasets,omitempty"`
}

// VMMetrics is the current use of one VM.
type VMMetrics struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	// CPUTimeSeconds and ResidentMemoryBytes are those of the bhyve
	// process, while the VM runs.
	CPUTimeSeconds      *float64 `json:"cpu_time_seconds,omitempty"`
	ResidentMemoryBytes *int64   `json:"resident_memory_bytes,omitempty"`
	// DiskUsedBytes is the space the VM holds in its pools, its
	// snapshots included; DiskReferencedBytes, what its disks reference
	// now.
	DiskUsedBytes       int64         `json:"disk_used_bytes"`
	DiskReferencedBytes int64         `json:"disk_referenced_bytes"`
	Disks               []DiskMetrics `json:"disks"`
	// Error says which values could not be read.
	Error string `json:"error,omitempty"`
}

// DiskMetrics is the space of one disk of a VM.
type DiskMetrics struct {
	Slot int    `json:"slot"`
	Pool string `json:"pool"`
	// UsedBytes includes the disk's snapshots on ZFS; on a directory
	// pool it is the file's allocated blocks.
	UsedBytes       int64 `json:"used_bytes"`
	ReferencedBytes int64 `json:"referenced_bytes"`
}
