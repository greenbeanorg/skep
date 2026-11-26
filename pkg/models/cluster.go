package models

import "time"

// Cluster is GET /api/v1/cluster: this node's view of its cluster (step
// 8c-2, D26). Nothing in it is secret: IDs, names, addresses and key pins
// are public within the cluster.
type Cluster struct {
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	// LeaderKnown says whether this node knows a leader now.
	LeaderKnown bool `json:"leader_known"`
	// StateMachineStoppedAt is the log index at which the replicated state
	// machine stopped, when it did (an entry this build cannot apply).
	StateMachineStoppedAt uint64            `json:"state_machine_stopped_at,omitempty"`
	Members               []ClusterMember   `json:"members"`
	Pendings              []ClusterPending  `json:"pending_admissions"`
	Removals              []string          `json:"removals,omitempty"`
	Rotations             []string          `json:"rotations,omitempty"`
	UnappliedRemoval      []string          `json:"unapplied_removal,omitempty"`
	Clocks                []ClusterClock    `json:"clocks,omitempty"`
	Health                Health            `json:"health"`
	Joining               *ClusterJoining   `json:"joining,omitempty"`
	Removed               *ClusterRemovedAs `json:"removed,omitempty"`
	Build                 string            `json:"build"`
}

// ClusterMember is one member of the cluster.
type ClusterMember struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Address string `json:"address"`
	Pin     string `json:"pin"`
	// NextPin is set while the member rotates its key.
	NextPin string `json:"next_pin,omitempty"`
	// Voter says whether the member votes in the Raft configuration.
	Voter bool `json:"voter"`
}

// ClusterPending is an admitted host that has not joined.
type ClusterPending struct {
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	Pin       string    `json:"pin"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ClusterClock is a member's clock as this node measured it.
type ClusterClock struct {
	NodeID            string `json:"node_id"`
	Name              string `json:"name"`
	Known             bool   `json:"known"`
	OffsetMillis      int64  `json:"offset_ms,omitempty"`
	UncertaintyMillis int64  `json:"uncertainty_ms,omitempty"`
	RoundTripMillis   int64  `json:"round_trip_ms,omitempty"`
	Note              string `json:"note,omitempty"`
}

// ClusterJoining is set while this node joins a cluster.
type ClusterJoining struct {
	Phase     string `json:"phase"`
	ClusterID string `json:"cluster_id"`
	Message   string `json:"message,omitempty"`
}

// ClusterRemovedAs is set on a node that was removed from its cluster.
type ClusterRemovedAs struct {
	ClusterID string `json:"cluster_id"`
	At        string `json:"at"`
	How       string `json:"how"`
}

// ClusterAdmitRequest is the body of POST /api/v1/cluster/admissions.
type ClusterAdmitRequest struct {
	// Request is the line "skep-cluster join-request" printed on the
	// host to admit.
	Request string `json:"request"`
}

// ClusterAdmission is the answer of an admission.
type ClusterAdmission struct {
	// Admission is the line to paste into "skep-cluster join" on the host.
	Admission string    `json:"admission"`
	ClusterID string    `json:"cluster_id"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	Pin       string    `json:"pin"`
	ExpiresAt time.Time `json:"expires_at"`
	OpID      string    `json:"op_id"`
}

// ClusterWithdrawal is the answer of DELETE
// /api/v1/cluster/admissions/{name}.
type ClusterWithdrawal struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	OpID   string `json:"op_id"`
}

// ClusterRemoveRequest is the body of POST
// /api/v1/cluster/members/{name}/remove. ClusterID is required: it must be
// the cluster's (409 cluster_id_mismatch otherwise).
type ClusterRemoveRequest struct {
	ClusterID string `json:"cluster_id"`
}

// ClusterRemoval is the answer of a removal: its first entry is committed
// and the leader finishes the removal by itself.
type ClusterRemoval struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	ClusterID string `json:"cluster_id"`
	OpID      string `json:"op_id"`
	// HandedOver is true when the member was the leader and handed the
	// leadership over first.
	HandedOver bool `json:"handed_over"`
}

// Guest inventory states (step 8d-1, D34).
const (
	// GuestCreating is a name reservation: a create or clone accepted
	// whose guest the inventory does not have yet.
	GuestCreating = "creating"
	// GuestPresent is a guest in the inventory.
	GuestPresent = "present"
)

// CodeTooManyGuests: the cluster's inventory holds its most guests and
// reservations (10000); nothing was created.
const CodeTooManyGuests = "too_many_guests"

// ClusterGuestOwner is the node that owns a guest.
type ClusterGuestOwner struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
}

// ClusterGuest is one item of GET /api/v1/cluster/guests: a guest of the
// replicated inventory, or a reservation (state creating, no ID).
// Flagged is true for every guest of a kind and name but the one added
// first.
type ClusterGuest struct {
	ID      string            `json:"id,omitempty"`
	Name    string            `json:"name"`
	Kind    string            `json:"kind"`
	Owner   ClusterGuestOwner `json:"owner"`
	State   string            `json:"state"`
	Flagged bool              `json:"flagged"`
	TaskID  string            `json:"task_id,omitempty"`
	Time    time.Time         `json:"time"`
}

// ClusterGuestList is the answer of GET /api/v1/cluster/guests.
type ClusterGuestList struct {
	ClusterID   string         `json:"cluster_id"`
	GuestsIndex uint64         `json:"guests_index"`
	Items       []ClusterGuest `json:"items"`
}
