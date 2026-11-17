package models

import "time"

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
	// CodeMlockRequiresMemoryLimit: mlock without an RCTL memory limit
	// (decision 9).
	CodeMlockRequiresMemoryLimit = "mlock_requires_memory_limit"
	// CodeContainerStartFailed: jail(8) or /etc/rc failed, or the jail's
	// parameters were not the baseline; the message says which.
	CodeContainerStartFailed = "container_start_failed"
)

// ContainerOptions are the opt-ins of decision 9 (amendment f dropped
// allow_raw_sockets).
type ContainerOptions struct {
	// SysVIPC gives the jail its own System V shared memory, semaphores,
	// and message queues.
	SysVIPC bool `json:"sysvipc"`
	// Mlock lets the jail lock memory. It needs a memory limit.
	Mlock bool `json:"mlock"`
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
	// SSHAuthorizedKey is root's SSH public key, if one was given.
	SSHAuthorizedKey string `json:"ssh_authorized_key,omitempty"`
	// Provisioned is true once container.create finished.
	Provisioned bool `json:"provisioned"`
	// StartedOnce is true from the first start on; Skep then never writes
	// into the jail's tree again (decision 13).
	StartedOnce  bool   `json:"started_once"`
	DesiredState string `json:"desired_state"`
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
	// SSHAuthorizedKey is one OpenSSH public key line, without options.
	SSHAuthorizedKey string `json:"ssh_authorized_key,omitempty"`
}

// ContainerUpdate changes a stopped jail. A field left out is unchanged;
// an empty ssh_authorized_key removes the key.
type ContainerUpdate struct {
	Hostname         *string           `json:"hostname,omitempty"`
	Options          *ContainerOptions `json:"options,omitempty"`
	SSHAuthorizedKey *string           `json:"ssh_authorized_key,omitempty"`
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
