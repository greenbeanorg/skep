package models

// Guest-port anti-spoofing (ADR 0011). The error codes, health checks,
// and events of the per-port netgraph filters. No API field, no
// configuration key, no migration (answer 10).

// Error codes.
const (
	// CodePortFilterUnavailable: a guest with a NIC needs the netgraph
	// modules (netgraph, ng_socket, ng_ether, ng_bpf), and one is not
	// loaded; Skep loads no modules. Like epair_unavailable.
	CodePortFilterUnavailable = "port_filter_unavailable"
	// CodePortFilterFailed: a guest port's filter could not be installed
	// or verified; the guest does not start (or, for a running guest, the
	// port is detached from its bridge).
	CodePortFilterFailed = "port_filter_failed"
)

// Health checks.
const (
	// CheckPortFilters fails while a running guest's port has no complete
	// filter, or has been detached from its bridge because its filter
	// could not be kept (decision 5).
	CheckPortFilters = "port_filters"
	// CheckBridgeVLANUnfiltered warns while a bridge with a running guest
	// port has no VLAN filtering, or a guest port on it has no PVID
	// (decision 8, answer 6). It never stops or refuses a guest.
	CheckBridgeVLANUnfiltered = "bridge_vlan_unfiltered"
	// CheckNestedInterrupts is present only on a host running under a
	// hypervisor with VT-x virtual interrupt delivery on: nested bhyve can
	// freeze the host on the first VM start.
	CheckNestedInterrupts = "nested_interrupts"
)

// Events, logged with event=<name> and written to the audit log as
// port_filter.<missing|restored|detached>.
const (
	EventPortFilterMissing  = "port_filter_missing"
	EventPortFilterRestored = "port_filter_restored"
	EventPortFilterDetached = "port_filter_detached"
)
