// Package models holds the public data types shared by the Skep API, the
// agent, and API clients. The JSON shapes here are the ones documented in
// api/openapi.yaml.
package models

import (
	"errors"
	"time"

	"github.com/greenbeanorg/skep/pkg/version"
)

// ErrNotFound is returned by services when a requested resource does not exist.
var ErrNotFound = errors.New("resource not found")

// CapabilityStatus says whether a host capability can be used.
type CapabilityStatus string

// Capability statuses. Discovery reports one of these, with a reason, instead
// of failing when something cannot be determined.
const (
	// StatusAvailable means the capability was verified as usable.
	StatusAvailable CapabilityStatus = "available"
	// StatusUnavailable means the capability was verified as not usable.
	StatusUnavailable CapabilityStatus = "unavailable"
	// StatusUnknown means discovery could not determine the state.
	StatusUnknown CapabilityStatus = "unknown"
)

// Capability is the status of one host capability and the reason for it.
type Capability struct {
	Status CapabilityStatus `json:"status"`
	Reason string           `json:"reason"`
}

// Platform values describe what kind of host the agent runs on.
const (
	// PlatformFreeBSD is a native FreeBSD host, the only production platform.
	PlatformFreeBSD = "freebsd"
	// PlatformLinuxDev is a Linux development host. Never a production host.
	PlatformLinuxDev = "linux-dev"
	// PlatformUnsupported is any other operating system.
	PlatformUnsupported = "unsupported"
)

// OSInfo describes the host operating system.
type OSInfo struct {
	// Name is the OS name, for example "FreeBSD".
	Name string `json:"name"`
	// Version is the OS version, for example "15.1".
	Version string `json:"version"`
	// Release is the full release string, for example "15.1-RELEASE-p6".
	Release string `json:"release"`
	// Kernel is the kernel identification string.
	Kernel string `json:"kernel"`
}

// HostInfo identifies the host.
type HostInfo struct {
	Hostname     string `json:"hostname"`
	OS           OSInfo `json:"os"`
	Architecture string `json:"architecture"`
}

// CPUInfo describes the host processors.
type CPUInfo struct {
	Model string `json:"model"`
	// Cores is the number of physical cores.
	Cores int `json:"cores"`
	// Threads is the number of logical CPUs.
	Threads int `json:"threads"`
}

// MemoryInfo describes host memory in bytes.
type MemoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// BhyveInfo reports whether the bhyve hypervisor can be used.
type BhyveInfo struct {
	Capability
	// VMMLoaded is true when the vmm kernel module is loaded.
	VMMLoaded bool `json:"vmm_loaded"`
	// VMMLoadable is true when vmm.ko is present and could be loaded.
	VMMLoadable bool `json:"vmm_loadable"`
	// VMMDevice is the vmm device node that was found, if any.
	VMMDevice string `json:"vmm_device,omitempty"`
	// BinaryPath is the path of the bhyve binary, if found.
	BinaryPath string `json:"binary_path,omitempty"`
}

// QEMUBinary is one QEMU executable found on the host.
type QEMUBinary struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Version is empty when the binary did not report a parsable version.
	Version string `json:"version,omitempty"`
	// Error explains why the version is missing.
	Error string `json:"error,omitempty"`
}

// QEMUInfo reports whether QEMU can be used.
type QEMUInfo struct {
	Capability
	// Acceleration is always "tcg" when QEMU is available: there is no KVM
	// on FreeBSD and Skep never uses it.
	Acceleration string       `json:"acceleration,omitempty"`
	Binaries     []QEMUBinary `json:"binaries"`
}

// ZFSPool is one imported ZFS pool, as listed read-only by zpool(8).
type ZFSPool struct {
	Name           string `json:"name"`
	SizeBytes      uint64 `json:"size_bytes"`
	AllocatedBytes uint64 `json:"allocated_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	// FragmentationPercent is nil when the pool does not report it.
	FragmentationPercent *int `json:"fragmentation_percent"`
	// CapacityPercent is nil when the pool does not report it.
	CapacityPercent *int   `json:"capacity_percent"`
	Health          string `json:"health"`
}

// ZFSInfo reports whether ZFS can be used and which pools are imported.
type ZFSInfo struct {
	Capability
	ModuleLoaded  bool   `json:"module_loaded"`
	ModuleVersion string `json:"module_version,omitempty"`
	ZpoolPath     string `json:"zpool_path,omitempty"`
	// Pools is the list of imported pools. It is empty when ZFS is not
	// available or the list could not be read; see PoolsError.
	Pools []ZFSPool `json:"pools"`
	// PoolsError explains why the pool list could not be read.
	PoolsError string `json:"pools_error,omitempty"`
}

// Hypervisors groups the hypervisor engines.
type Hypervisors struct {
	Bhyve BhyveInfo `json:"bhyve"`
	QEMU  QEMUInfo  `json:"qemu"`
}

// StorageInfo groups the storage backends.
type StorageInfo struct {
	ZFS ZFSInfo `json:"zfs"`
}

// Hardware is the result of one read-only host discovery pass.
type Hardware struct {
	NodeID      string      `json:"node_id"`
	Platform    string      `json:"platform"`
	Host        HostInfo    `json:"host"`
	CPU         CPUInfo     `json:"cpu"`
	Memory      MemoryInfo  `json:"memory"`
	Hypervisors Hypervisors `json:"hypervisors"`
	Storage     StorageInfo `json:"storage"`
	// Warnings lists facts that could not be discovered. Discovery keeps
	// going when one probe fails and records the failure here.
	Warnings    []string  `json:"warnings"`
	CollectedAt time.Time `json:"collected_at"`
}

// CapabilitySummary is the short capability overview in the system document.
type CapabilitySummary struct {
	Bhyve Capability `json:"bhyve"`
	QEMU  Capability `json:"qemu"`
	ZFS   Capability `json:"zfs"`
}

// System describes the local Skep node.
type System struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
	// ProductionHost is true only on native FreeBSD outside dev mode.
	ProductionHost bool              `json:"production_host"`
	DevMode        bool              `json:"dev_mode"`
	OS             OSInfo            `json:"os"`
	Architecture   string            `json:"architecture"`
	Skep           version.Info      `json:"skep"`
	Capabilities   CapabilitySummary `json:"capabilities"`
	StartedAt      time.Time         `json:"started_at"`
	UptimeSeconds  int64             `json:"uptime_seconds"`
}

// Health statuses.
const (
	// HealthOK means every check passed.
	HealthOK = "ok"
	// HealthDegraded means at least one check failed.
	HealthDegraded = "degraded"
)

// HealthCheck is the result of one health check.
type HealthCheck struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Health is the agent health document.
type Health struct {
	Status        string                 `json:"status"`
	Version       string                 `json:"version"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	Checks        map[string]HealthCheck `json:"checks"`
}

// ErrorBody is the error payload used by every API error response.
type ErrorBody struct {
	// Code is a stable, machine-readable error code.
	Code string `json:"code"`
	// Message is a human-readable description.
	Message string `json:"message"`
	// RequestID correlates the response with the server logs.
	RequestID string `json:"request_id,omitempty"`
	// Details carries optional structured context.
	Details map[string]any `json:"details,omitempty"`
}

// ErrorResponse is the envelope of every API error response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// Error is an error that is safe to show to an API client: a stable code
// and a message written for the caller. The HTTP layer maps the code to a
// status. Two Errors match with errors.Is when their codes are equal.
type Error struct {
	Code    string
	Message string
	// Details carries optional structured context for the client.
	Details map[string]any
}

// Error implements error.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Error codes shared by the service layer and the API.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeResourceBusy        = "resource_busy"
	CodeIdempotencyConflict = "idempotency_key_conflict"
	CodeNotCancellable      = "not_cancellable"
	CodeQueueFull           = "queue_full"
	CodeShuttingDown        = "shutting_down"
)

// Invalid returns an invalid_request Error with the given message.
func Invalid(message string) *Error {
	return &Error{Code: CodeInvalidRequest, Message: message}
}

// TaskStatus is the state of a task.
type TaskStatus string

// Task statuses. Succeeded, failed, cancelled, and interrupted are terminal.
const (
	TaskPending     TaskStatus = "pending"
	TaskRunning     TaskStatus = "running"
	TaskSucceeded   TaskStatus = "succeeded"
	TaskFailed      TaskStatus = "failed"
	TaskCancelled   TaskStatus = "cancelled"
	TaskInterrupted TaskStatus = "interrupted"
)

// Terminal reports whether a task in this status will never change again.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskSucceeded, TaskFailed, TaskCancelled, TaskInterrupted:
		return true
	}
	return false
}

// Valid reports whether s is one of the six task statuses.
func (s TaskStatus) Valid() bool {
	return s == TaskPending || s == TaskRunning || s.Terminal()
}

// TaskActor is who a task was requested for.
type TaskActor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TaskError explains why a task did not succeed.
type TaskError struct {
	// Code is stable and machine-readable, for example "timeout".
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Task is one asynchronous operation performed by the agent.
type Task struct {
	ID           string     `json:"id"`
	NodeID       string     `json:"node_id"`
	Type         string     `json:"type"`
	Status       TaskStatus `json:"status"`
	ResourceType string     `json:"resource_type,omitempty"`
	ResourceID   string     `json:"resource_id,omitempty"`
	Actor        TaskActor  `json:"actor"`
	RequestID    string     `json:"request_id,omitempty"`
	// Cancellable says whether this task can be cancelled while running.
	Cancellable       bool       `json:"cancellable"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	// Result is the JSON result of a succeeded task.
	Result     any        `json:"result,omitempty"`
	Error      *TaskError `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Task event kinds.
const (
	// TaskEventState is a state change, written by the task engine.
	TaskEventState = "state"
	// TaskEventProgress is a progress message, written by the task itself.
	TaskEventProgress = "progress"
)

// TaskEvent is one entry in a task's history.
type TaskEvent struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Data      any       `json:"data,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskDetail is a task together with its event history.
type TaskDetail struct {
	Task
	Events []TaskEvent `json:"events"`
}

// TaskList is one page of tasks, newest first.
type TaskList struct {
	Items []Task `json:"items"`
	// NextCursor is passed back as the cursor parameter to get the next
	// page. It is empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// TaskListOptions selects a page of tasks.
type TaskListOptions struct {
	// Status filters by status when not empty.
	Status TaskStatus
	// Limit is the page size. Zero means the default.
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}
