package models

import (
	"fmt"
	"time"
)

// Jails (ADR 0010 decisions 8, 9, 12, and 13). A container is a FreeBSD
// jail on a ZFS clone of a template's @skep-base, named skep-<id>, with its
// own network stack.

// Container tasks.
const (
	TaskContainerCreate  = "container.create"
	TaskContainerUpdate  = "container.update"
	TaskContainerDelete  = "container.delete"
	TaskContainerStart   = "container.start"
	TaskContainerStop    = "container.stop"
	TaskContainerRestart = "container.restart"
	// Snapshots and linked clones of jails (decisions 2 and 3).
	TaskContainerSnapshotCreate   = "container.snapshot.create"
	TaskContainerSnapshotRollback = "container.snapshot.rollback"
	TaskContainerSnapshotDelete   = "container.snapshot.delete"
	TaskContainerClone            = "container.clone"
)

// ContainerResource is the resource type of the container tasks.
const ContainerResource = "container"

// Container states: the same words as a VM's.
const (
	ContainerUnknown  = "unknown"
	ContainerStopped  = "stopped"
	ContainerRunning  = "running"
	ContainerStarting = "starting"
	ContainerStopping = "stopping"
	ContainerFailed   = "failed"
)

// Container actions.
const (
	ContainerActionStart   = "start"
	ContainerActionStop    = "stop"
	ContainerActionRestart = "restart"
)

// Stop timeouts of a jail (decision 8): the configured default, and the
// most a request may ask for.
const (
	DefaultContainerStopTimeoutSeconds = 10
	MaxContainerStopTimeoutSeconds     = 300
)

// Error codes of the container operations.
const (
	// CodeContainerNameTaken: another container has the name.
	CodeContainerNameTaken = "container_name_taken"
	// CodeContainerNotStopped: the jail is running.
	CodeContainerNotStopped = "container_not_stopped"
	// CodeContainerNotRunning: the jail is not running.
	CodeContainerNotRunning = "container_not_running"
	// CodeContainerNotProvisioned: the create task never finished; the
	// container can only be deleted.
	CodeContainerNotProvisioned = "container_not_provisioned"
	// CodeContainerStarted: the change would write into the tree of a
	// jail that has started at least once (decision 13).
	CodeContainerStarted = "container_started"
	// CodeContainerSSHUnavailable: an SSH key was given for a jail whose
	// template has no sshd (amendment g).
	CodeContainerSSHUnavailable = "container_ssh_unavailable"
	// CodeMlockRequiresMemoryLimit: mlock without an RCTL locked-memory
	// limit, or without RACCT (decision 9, amendment j).
	CodeMlockRequiresMemoryLimit = "mlock_requires_memory_limit"
	// CodeContainerStartFailed: jail(8) or /etc/rc failed, or the jail's
	// parameters were not the baseline; the message says which.
	CodeContainerStartFailed = "container_start_failed"
	// CodeEpairUnavailable: a jail with a NIC needs if_epair, and the
	// module is not loaded; Skep loads no modules (decision 10).
	CodeEpairUnavailable = "epair_unavailable"
	// CodeDevfsRulesetConflict: Skep's devfs ruleset number for DHCP jails
	// holds rules Skep did not put there (amendment h).
	CodeDevfsRulesetConflict = "devfs_ruleset_conflict"
	// CodeRctlUnavailable: a CPU, memory, process, or file limit on a
	// host where RACCT is disabled (decision 11).
	CodeRctlUnavailable = "rctl_unavailable"
	// CodeCloneAddressingRequired: a clone of a jail with a static
	// address must give each such NIC new addressing (amendment k).
	CodeCloneAddressingRequired = "clone_addressing_required"
	// CodeContainerAddressInUse: a running Skep jail on the same bridge
	// holds the jail's static IPv4 address (amendment k).
	CodeContainerAddressInUse = "container_address_in_use"
	// CodeContainerMACInvalid: a NIC's MAC address is multicast, not
	// locally administered, or all zeroes (amendment n, SR-03).
	CodeContainerMACInvalid = "container_mac_invalid"
	// CodeContainerMACInUse: at start, the NIC's MAC address is already
	// present on the target bridge (its own address, a member's, or one it
	// has learnt) or held by another Skep jail or VM (amendment n, SR-03).
	CodeContainerMACInUse = "container_mac_in_use"
	// CodeContainerQuarantined: a start refused the jail before any of its
	// code ran (the /dev guard or a parameter check), and the removal that
	// followed failed. The jail is recorded failed; start and stop are
	// refused, Skep never runs its exec.stop or /etc/rc.shutdown, and
	// delete or the agent's start-up sweep remove it without them
	// (amendment n, SR-06).
	CodeContainerQuarantined = "container_quarantined"
	// CodeContainerRemoveFailed: a stop or delete could not remove the
	// jail. jail -r failed (or timed out, or left the jail), and the
	// removal without any of the jail's code that followed, jail -R,
	// failed too. The message has the cleaned last stderr line of both
	// (ADR 0010 amendment u).
	CodeContainerRemoveFailed = "container_remove_failed"
	// CodeContainerLockedMemoryAboveMax: limits.locked_memory is more
	// than containers.max_locked_memory_bytes, the host's total for all
	// running jails (amendment o, SR-04).
	CodeContainerLockedMemoryAboveMax = "container_locked_memory_above_max"
	// CodeContainerLockedMemoryExhausted: at start, the jail's
	// locked_memory plus that of the jails already running (or starting)
	// is more than containers.max_locked_memory_bytes (amendment o,
	// SR-04).
	CodeContainerLockedMemoryExhausted = "container_locked_memory_exhausted"
	// CodeContainerDiskQuotaBelowUsage: an update's limits.disk_quota is
	// below what the jail's root already references, or a create's is
	// below the template's size, so ZFS would refuse the refquota
	// (amendments w and x). Refused at submission; a task that meets it
	// (the root grew after the request was accepted) fails with it too.
	CodeContainerDiskQuotaBelowUsage = "container_disk_quota_below_usage"
)

// DiskQuotaBelowUsage is the container_disk_quota_below_usage refusal of
// a disk quota below what the jail's root already references (ADR 0010
// amendment w). referenced is ZFS's referenced value of the root; the
// quota (refquota) does not count snapshots.
func DiskQuotaBelowUsage(containerID string, quota, referenced int64) *Error {
	return &Error{Code: CodeContainerDiskQuotaBelowUsage,
		Message: fmt.Sprintf("limits.disk_quota (%d bytes) is below what the jail's root already references (%d bytes); give at least that much, or remove data inside the jail first", quota, referenced),
		Details: map[string]any{"container_id": containerID, "disk_quota": quota, "referenced_bytes": referenced}}
}

// DiskQuotaBelowTemplate is the container_disk_quota_below_usage refusal
// of a create whose disk quota is below the template's size: a new jail's
// root references the template's data from the start (ADR 0010
// amendment x).
func DiskQuotaBelowTemplate(templateID string, quota, referenced int64) *Error {
	return &Error{Code: CodeContainerDiskQuotaBelowUsage,
		Message: fmt.Sprintf("limits.disk_quota (%d bytes) is below the template's size (%d bytes), which a new jail's root references from the start; give at least that much", quota, referenced),
		Details: map[string]any{"template_id": templateID, "disk_quota": quota, "referenced_bytes": referenced}}
}

// IPv4 addressing modes of a jail NIC (decision 10).
const (
	IPv4DHCP   = "dhcp"
	IPv4Static = "static"
)

// Bounds of a jail's network configuration.
const (
	// MaxContainerNICs is the most NICs a jail may have.
	MaxContainerNICs = 8
	// MaxContainerNameservers is resolv.conf's MAXNS.
	MaxContainerNameservers = 3
)

// ContainerIPv4 is how a jail NIC gets its IPv4 address: by DHCP, or a
// static address in CIDR form with an optional gateway (the default
// route; one NIC at most has one).
type ContainerIPv4 struct {
	Mode    string `json:"mode"`
	Address string `json:"address,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// ContainerNIC is one VNET interface of a jail: an epair whose host end
// joins Bridge and whose jail end moves into the jail and is named
// Interface there: skep<slot>, the same for every jail (decision 10,
// amendment k). The addressing is written into the jail's rc.conf before
// its first start; after that it describes what Skep wrote, and changing
// it is the jail's own business.
type ContainerNIC struct {
	ID         string        `json:"id"`
	Slot       int           `json:"slot"`
	Bridge     string        `json:"bridge"`
	MACAddress string        `json:"mac_address"`
	Interface  string        `json:"interface"`
	IPv4       ContainerIPv4 `json:"ipv4"`
	// HostInterface is the epair's host end, while the jail runs.
	HostInterface string `json:"host_interface,omitempty"`
}

// ContainerNICSpec asks for one NIC. MACAddress is generated when empty;
// IPv4 defaults to DHCP.
type ContainerNICSpec struct {
	Bridge     string         `json:"bridge"`
	MACAddress string         `json:"mac_address,omitempty"`
	IPv4       *ContainerIPv4 `json:"ipv4,omitempty"`
}

// ContainerOptions are the opt-ins of decision 9 (amendment f dropped
// allow_raw_sockets).
type ContainerOptions struct {
	// SysVIPC gives the jail its own System V shared memory, semaphores,
	// and message queues.
	SysVIPC bool `json:"sysvipc"`
	// Mlock lets the jail lock memory. It needs a locked-memory limit
	// (limits.locked_memory, amendment j).
	Mlock bool `json:"mlock"`
}

// ContainerLimits are a jail's resource limits (decision 11); zero or
// absent means none. Memory, CPUPercent, MaxProcesses, OpenFiles, and
// LockedMemory are RCTL rules with the deny action on the jail and need RACCT
// (rctl_unavailable otherwise); DiskQuota is ZFS refquota on the jail's
// root, so its snapshots do not count against it, and works without
// RACCT.
type ContainerLimits struct {
	// Memory is RCTL memoryuse: the jail's resident memory. It is a soft
	// limit (amendment j): FreeBSD accounts resident memory and does not
	// refuse an allocation past it.
	Memory ByteSize `json:"memory,omitempty"`
	// CPUPercent is RCTL pcpu: 100 is one CPU, so a jail is throttled to
	// CPUPercent/100 CPUs. At most 100 per CPU of the host.
	CPUPercent int `json:"cpu_percent,omitempty"`
	// MaxProcesses is RCTL maxproc; OpenFiles is RCTL openfiles.
	MaxProcesses int `json:"max_processes,omitempty"`
	OpenFiles    int `json:"open_files,omitempty"`
	// DiskQuota is the most the jail's root may reference, the data it
	// shares with its template included.
	DiskQuota ByteSize `json:"disk_quota,omitempty"`
	// LockedMemory is RCTL memorylocked: the memory the jail may lock
	// with mlock(2). The mlock opt-in needs it (amendment j).
	LockedMemory ByteSize `json:"locked_memory,omitempty"`
}

// Bounds of the jail limits.
const (
	MinContainerMemory    = 16 << 20
	MinContainerDiskQuota = 64 << 20
	// MaxContainerProcesses and MaxContainerOpenFiles bound the RCTL
	// counts; the kernel's own limits apply as well.
	MaxContainerProcesses = 1 << 20
	MaxContainerOpenFiles = 1 << 24
)

// RCTL reports whether the host can enforce RCTL limits.
func (l ContainerLimits) RCTL() bool {
	return l.Memory > 0 || l.CPUPercent > 0 || l.MaxProcesses > 0 || l.OpenFiles > 0 || l.LockedMemory > 0
}

// ContainerCapabilities is what the host offers jails, in the system
// document: whether RCTL limits can be enforced, and the bound of a CPU
// limit. A disk quota needs only ZFS.
type ContainerCapabilities struct {
	// RCTL is kern.racct.enable. Without it, a CPU, memory, process, or
	// file limit is refused with rctl_unavailable and a jail that has one
	// does not start.
	RCTL bool `json:"rctl"`
	// MaxCPUPercent is 100 per CPU of the host.
	MaxCPUPercent int `json:"max_cpu_percent"`
}

// Container is one jail.
type Container struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	// Template and its name, and the pool both live in.
	TemplateID   string `json:"template_id"`
	TemplateName string `json:"template_name"`
	Pool         string `json:"pool"`
	// Release is the template's userland release.
	Release string           `json:"release,omitempty"`
	Options ContainerOptions `json:"options"`
	Limits  ContainerLimits  `json:"limits"`
	// SSHAuthorizedKey is root's SSH public key, if one was given.
	SSHAuthorizedKey string `json:"ssh_authorized_key,omitempty"`
	// NICs are the jail's VNET interfaces, by slot; Nameservers go into
	// its resolv.conf (static addressing only).
	NICs        []ContainerNIC `json:"nics"`
	Nameservers []string       `json:"nameservers,omitempty"`
	// Provisioned is true once container.create finished.
	Provisioned bool `json:"provisioned"`
	// StartedOnce is true from the first start on; Skep then never writes
	// into the jail's tree again (decision 13).
	StartedOnce bool `json:"started_once"`
	// ClonedFromSnapshotID is the snapshot a linked clone was made from
	// (decision 3); deleting it, or its jail, is refused while the clone
	// exists.
	ClonedFromSnapshotID string `json:"cloned_from_snapshot_id,omitempty"`
	DesiredState         string `json:"desired_state"`
	// State, JID, and StateDetail are observed.
	State       string     `json:"state"`
	JID         *int       `json:"jid,omitempty"`
	StateDetail string     `json:"state_detail,omitempty"`
	ObservedAt  *time.Time `json:"observed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ContainerCreate is a request to create a jail from a template.
type ContainerCreate struct {
	Name string `json:"name"`
	// Hostname defaults to the name.
	Hostname   string            `json:"hostname,omitempty"`
	TemplateID string            `json:"template_id"`
	Options    *ContainerOptions `json:"options,omitempty"`
	Limits     *ContainerLimits  `json:"limits,omitempty"`
	// SSHAuthorizedKey is one OpenSSH public key line, without options.
	SSHAuthorizedKey string `json:"ssh_authorized_key,omitempty"`
	// NICs are slots 0, 1, ... in order. Nameservers are allowed only
	// when no NIC uses DHCP (dhclient writes resolv.conf itself).
	NICs        []ContainerNICSpec `json:"nics,omitempty"`
	Nameservers []string           `json:"nameservers,omitempty"`
}

// ContainerUpdate changes a stopped jail. A field left out is unchanged;
// an empty ssh_authorized_key removes the key, an empty nics list every
// NIC.
type ContainerUpdate struct {
	Hostname *string           `json:"hostname,omitempty"`
	Options  *ContainerOptions `json:"options,omitempty"`
	// Limits replaces every limit: one left out of the object is removed.
	Limits           *ContainerLimits `json:"limits,omitempty"`
	SSHAuthorizedKey *string          `json:"ssh_authorized_key,omitempty"`
	// NICs replaces the whole list. After the first start only the bridge
	// and MAC of existing slots may change: the addressing and the
	// interface names are in the jail's rc.conf.
	NICs        *[]ContainerNICSpec `json:"nics,omitempty"`
	Nameservers *[]string           `json:"nameservers,omitempty"`
}

// ContainerAction asks for start, stop, or restart. Timeout applies to
// stop and restart: the seconds between SIGTERM and SIGKILL.
type ContainerAction struct {
	Action  string `json:"action"`
	Timeout *int   `json:"timeout,omitempty"`
}

// ContainerCreated is the reply to a create: the container, not yet
// provisioned, and its task.
type ContainerCreated struct {
	Container Container `json:"container"`
	Task      Task      `json:"task"`
}

// ContainerClone is a request for a linked clone of a jail (decision 3):
// a new jail whose root is a ZFS clone of one snapshot of the source's
// root. Without SnapshotID the task takes a snapshot first, named after
// the clone, and leaves it behind. The clone has the source's template,
// options, limits, SSH key, and NIC slots, with new MAC addresses.
//
// NICs, when given, has one entry per NIC of the source, in slot order;
// an entry's empty fields keep the source's bridge and addressing. Every
// NIC of the source with a static address needs new addressing here
// (clone_addressing_required), and new addressing can be written only
// into a clone whose tree has never run code (container_started)
// (amendment k, decision 13). Nameservers, when given, replace the
// source's, under the same rule.
type ContainerClone struct {
	Name string `json:"name"`
	// Hostname defaults to the name, as for a create.
	Hostname    string             `json:"hostname,omitempty"`
	SnapshotID  string             `json:"snapshot_id,omitempty"`
	NICs        []ContainerNICSpec `json:"nics,omitempty"`
	Nameservers []string           `json:"nameservers,omitempty"`
}

// ContainerList is a page of containers, ordered by name.
type ContainerList struct {
	Items      []Container `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ContainerListOptions selects a page.
type ContainerListOptions struct {
	Limit  int
	Cursor string
}

// ContainerConsoleLog is the end of a jail's console.log.
type ContainerConsoleLog struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
