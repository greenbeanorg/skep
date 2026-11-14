package models

import "time"

// Owner kinds of a dataset in the inventory (ADR 0010 decision 1).
const (
	// DatasetOwnerVM, DatasetOwnerContainer, and DatasetOwnerTemplate
	// name a resource in the agent's database.
	DatasetOwnerVM        = "vm"
	DatasetOwnerContainer = "container"
	DatasetOwnerTemplate  = "template"
	// DatasetOwnerSkep is a dataset Skep creates to hold others:
	// <pool>/<root>, <root>/vm, <root>/jail, <root>/templates.
	DatasetOwnerSkep = "skep"
	// DatasetOwnerUnowned is anything else under <pool>/<root>: an
	// interrupted create, a leftover, a hand-made dataset. It is listed
	// and flagged, never removed automatically.
	DatasetOwnerUnowned = "unowned"
)

// DatasetOwner is what a dataset belongs to.
type DatasetOwner struct {
	Kind string `json:"kind"`
	// ID is the resource's ID for vm, container, and template.
	ID string `json:"id,omitempty"`
	// Reason says why a dataset is unowned.
	Reason string `json:"reason,omitempty"`
}

// Dataset is one filesystem, volume, or snapshot under <pool>/<root> of an
// allow-listed pool, as observed when it was listed.
type Dataset struct {
	// Name is the full ZFS name.
	Name string `json:"name"`
	Pool string `json:"pool"`
	// Type is "filesystem", "volume", or "snapshot".
	Type  string       `json:"type"`
	Owner DatasetOwner `json:"owner"`
	// SnapshotID is set on a snapshot Skep took.
	SnapshotID      string `json:"snapshot_id,omitempty"`
	UsedBytes       int64  `json:"used_bytes"`
	ReferencedBytes int64  `json:"referenced_bytes"`
	// VolSizeBytes, RefReservationBytes, and VolBlockSize are set on a
	// volume; a thin volume has no reservation.
	VolSizeBytes        int64 `json:"volsize_bytes,omitempty"`
	RefReservationBytes int64 `json:"refreservation_bytes,omitempty"`
	VolBlockSize        int   `json:"volblocksize,omitempty"`
	// Origin is the snapshot a linked clone was made from; Clones, on a
	// snapshot, the datasets cloned from it.
	Origin string   `json:"origin,omitempty"`
	Clones []string `json:"clones,omitempty"`
	// Encryption, KeyStatus, Compression, and CompressRatio are effective
	// values, which Skep inherits and never sets (ADR 0010 decision 5).
	// KeyStatus is empty for a dataset that is not encrypted.
	Encryption    string `json:"encryption,omitempty"`
	KeyStatus     string `json:"keystatus,omitempty"`
	Compression   string `json:"compression,omitempty"`
	CompressRatio string `json:"compressratio,omitempty"`
}

// DatasetListOptions filters and pages the inventory.
type DatasetListOptions struct {
	// Pool limits the list to one allow-listed pool.
	Pool   string
	Limit  int
	Cursor string
}

// DatasetList is a page of the inventory, ordered by pool and name.
type DatasetList struct {
	Items      []Dataset `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	// UnavailablePools lists the allow-listed pools that could not be
	// read, and so are missing from the list.
	UnavailablePools []string `json:"unavailable_pools,omitempty"`
}

// Snapshot tasks and the clone task (ADR 0010 decisions 2 and 3).
const (
	TaskVMSnapshotCreate   = "vm.snapshot.create"
	TaskVMSnapshotRollback = "vm.snapshot.rollback"
	TaskVMSnapshotDelete   = "vm.snapshot.delete"
	TaskVMClone            = "vm.clone"
)

// Snapshot consistency (ADR 0010 decision 2a). There is no guest agent,
// so no snapshot is called application-consistent.
const (
	// SnapshotConsistent: taken while the resource was stopped.
	SnapshotConsistent = "consistent"
	// SnapshotCrashConsistent: taken while it ran; what a power cut
	// would leave on disk.
	SnapshotCrashConsistent = "crash_consistent"
)

// DefaultMaxSnapshotsPerResource is storage.max_snapshots_per_resource
// when it is not set.
const DefaultMaxSnapshotsPerResource = 64

// CodeSnapshotNameTaken is returned when a resource already has a
// snapshot with the requested name.
const CodeSnapshotNameTaken = "snapshot_name_taken"

// CodeSnapshotNotProvisioned is returned for a snapshot whose task never
// finished: it can only be deleted.
const CodeSnapshotNotProvisioned = "snapshot_not_provisioned"

// SnapshotClone names a linked clone that depends on a snapshot.
type SnapshotClone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SnapshotObserved is what the agent last saw of a snapshot in ZFS.
type SnapshotObserved struct {
	Exists bool `json:"exists"`
	// UsedBytes is the space the snapshot alone holds, summed over the
	// resource's datasets.
	UsedBytes int64      `json:"used_bytes"`
	At        *time.Time `json:"at,omitempty"`
}

// Snapshot is one snapshot of a VM's (or, later, a jail's) disks: one
// recursive ZFS snapshot of every dataset of the resource.
type Snapshot struct {
	ID           string `json:"id"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	// Consistency is "consistent" or "crash_consistent".
	Consistency string `json:"consistency"`
	// Provisioned is false while the snapshot task runs, and after it
	// was interrupted.
	Provisioned bool              `json:"provisioned"`
	CreatedAt   time.Time         `json:"created_at"`
	Observed    *SnapshotObserved `json:"observed,omitempty"`
	// Clones are the linked clones made from this snapshot. While there
	// are any, the snapshot cannot be deleted.
	Clones []SnapshotClone `json:"clones"`
}

// SnapshotList is the list of a resource's snapshots, oldest first.
type SnapshotList struct {
	Items []Snapshot `json:"items"`
}

// SnapshotCreate is the body of POST /vms/{id}/snapshots.
type SnapshotCreate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SnapshotCreated is the answer to a snapshot request: the snapshot, not
// yet provisioned, and the task that takes it.
type SnapshotCreated struct {
	Snapshot Snapshot `json:"snapshot"`
	Task     Task     `json:"task"`
}

// SnapshotRollback is the body of POST /vms/{id}/snapshots/{sid}/rollback.
type SnapshotRollback struct {
	// DestroyNewer allows the rollback to an older snapshot, which
	// destroys every newer one.
	DestroyNewer bool `json:"destroy_newer,omitempty"`
}

// VMClone is the body of POST /vms/{id}/clone. Without a SnapshotID, the
// task takes a snapshot of the source first, named after the clone, and
// leaves it behind.
type VMClone struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
}
