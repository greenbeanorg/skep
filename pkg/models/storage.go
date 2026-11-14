package models

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
