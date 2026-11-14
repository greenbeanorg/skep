// Package models holds the public data types shared by the Skep API, the
// agent, and API clients. The JSON shapes here are the ones documented in
// api/openapi.yaml.
package models

import (
	"context"
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
	// Bhyve is what the bhyve engine can do on this host: the detail
	// behind Capabilities.Bhyve, with the limits a VM is checked against.
	Bhyve         *BhyveCapabilities `json:"bhyve,omitempty"`
	StartedAt     time.Time          `json:"started_at"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	// API describes the API server that answered. The agent leaves it
	// out; skep-api fills it in.
	API *APIInfo `json:"api,omitempty"`
	// Health is the full health document, with every check, including
	// degraded ones. The agent leaves it out; skep-api fills it in.
	Health *Health `json:"health,omitempty"`
}

// APIInfo describes the skep-api process that served a request.
type APIInfo struct {
	TLS TLSInfo `json:"tls"`
}

// TLSInfo describes the certificate skep-api serves. Nothing in it is
// secret: a client sees the same in the handshake.
type TLSInfo struct {
	// Source is "self-signed" (generated and renewed by skep-api) or
	// "operator" (supplied in the configuration; Skep never renews it).
	Source string `json:"source"`
	// NotAfter is when the certificate expires.
	NotAfter time.Time `json:"not_after"`
	// PublicKeySHA256 is the SHA-256 of the certificate's
	// SubjectPublicKeyInfo, as "sha256:<hex>". Clients pin this value; it
	// does not change when the certificate is renewed.
	PublicKeySHA256 string `json:"public_key_sha256"`
}

// Health statuses, of a whole document and of one check. Only
// HealthUnavailable means the service cannot serve requests; the public
// health endpoint answers 503 for it and 200 for the others.
const (
	// HealthOK means every check passed.
	HealthOK = "ok"
	// HealthDegraded means a non-critical check failed: the service
	// still serves requests, but something needs an operator, for
	// example a late or damaged audit archive.
	HealthDegraded = "degraded"
	// HealthUnavailable means a critical check failed: the service cannot
	// serve requests, for example because the agent or its database
	// cannot be reached.
	HealthUnavailable = "unavailable"
)

// HealthCheck is the result of one health check.
type HealthCheck struct {
	// Status is HealthOK, or, when the check failed, HealthUnavailable
	// for a critical check and HealthDegraded for any other.
	Status string `json:"status"`
	// Critical is true when the service cannot serve requests without
	// this check passing.
	Critical bool   `json:"critical"`
	Error    string `json:"error,omitempty"`
}

// Record adds the result of a check to the document and updates its
// status: problem is "" when the check passed. The document's status is
// the worst of its checks'.
func (h *Health) Record(name string, critical bool, problem string) {
	check := HealthCheck{Status: HealthOK, Critical: critical}
	if problem != "" {
		check.Status, check.Error = HealthDegraded, problem
		if critical {
			check.Status = HealthUnavailable
		}
	}
	if h.Checks == nil {
		h.Checks = map[string]HealthCheck{}
	}
	h.Checks[name] = check
	h.Status = WorseHealth(h.Status, check.Status)
}

// WorseHealth returns the worse of two health statuses. An empty or
// unknown status counts as HealthOK.
func WorseHealth(a, b string) string {
	rank := func(s string) int {
		switch s {
		case HealthUnavailable:
			return 2
		case HealthDegraded:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	if rank(a) == 0 {
		return HealthOK
	}
	return a
}

// HealthProbe is one additional health check of a service: it returns ""
// when all is well, or a short description of the problem that is safe to
// show to an authenticated caller. Causes belong in the log.
type HealthProbe func(ctx context.Context) string

// Names of the audit archive health checks (ADR 0007, amendment d).
const (
	// CheckAuditArchive is the agent's check of its operation audit log's
	// archive.
	CheckAuditArchive = "audit_archive"
	// CheckAccessAuditArchive is skep-api's check of its access audit
	// log's archive.
	CheckAccessAuditArchive = "access_audit_archive"
)

// Health is the agent health document.
type Health struct {
	Status        string                 `json:"status"`
	Version       string                 `json:"version"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	Checks        map[string]HealthCheck `json:"checks"`
}

// HealthStatus is what the health endpoint returns to a caller without a
// session: the status and nothing else. Monitoring that must notice a
// degraded component checks this field, not only the HTTP status, which
// is 503 for HealthUnavailable alone.
type HealthStatus struct {
	Status string `json:"status"`
}

// User is an account, as the API shows it. It never includes the password
// hash.
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	// Role is the name of the user's role.
	Role              string     `json:"role"`
	Disabled          bool       `json:"disabled"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
	PasswordChangedAt *time.Time `json:"password_changed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Session kinds.
const (
	SessionCookie = "cookie"
	SessionBearer = "bearer"
)

// LoginRequest is the body of POST /auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Session is "cookie" (the default) or "bearer". A cookie login sets
	// the session cookie and returns no token. A bearer login returns
	// the token in the body and sets no cookie.
	Session string `json:"session,omitempty"`
}

// SessionInfo describes a session. It never includes the token.
type SessionInfo struct {
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is the absolute end of the session.
	ExpiresAt time.Time `json:"expires_at"`
	// IdleTimeoutSeconds is how long the session survives without use.
	IdleTimeoutSeconds int64 `json:"idle_timeout_seconds"`
}

// SessionResponse is the body of a successful login and of
// GET /auth/session.
type SessionResponse struct {
	User    User        `json:"user"`
	Session SessionInfo `json:"session"`
	// Permissions are the permission strings of the user's role. "*"
	// means every permission.
	Permissions []string `json:"permissions"`
	// Token is the session token. It is present only in the response to
	// a login that asked for a bearer session.
	Token string `json:"token,omitempty"`
}

// PasswordChange is the body of PUT /auth/password.
type PasswordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// UserList is the list of all users, ordered by username.
type UserList struct {
	Items []User `json:"items"`
}

// NewUser is the body of POST /users.
type NewUser struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Password    string `json:"password"`
	// Role is the name of a role. It defaults to "viewer".
	Role string `json:"role,omitempty"`
}

// UserUpdate is the body of PUT /users/{id}. Fields that are left out are
// not changed.
type UserUpdate struct {
	DisplayName *string `json:"display_name,omitempty"`
	Role        *string `json:"role,omitempty"`
	Disabled    *bool   `json:"disabled,omitempty"`
	// Password sets a new password for the user. It ends all of that
	// user's sessions.
	Password *string `json:"password,omitempty"`
}

// Role is a named set of permissions.
type Role struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Permissions are "resource:action" strings. "*" means every
	// permission.
	Permissions []string `json:"permissions"`
	// BuiltIn roles cannot be changed or deleted.
	BuiltIn bool `json:"built_in"`
}

// RoleList is the list of all roles.
type RoleList struct {
	Items []Role `json:"items"`
}

// AuditEntry is one entry of the access audit log: an authentication
// event, an authorization decision, or an administrative change.
type AuditEntry struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	// ActorID is empty when no known user was involved.
	ActorID      string `json:"actor_id,omitempty"`
	ActorName    string `json:"actor_name"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
	// Outcome is "success", "failure", or "denied".
	Outcome   string         `json:"outcome"`
	SourceIP  string         `json:"source_ip,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// AuditList is one page of the access audit log, newest first.
type AuditList struct {
	Items []AuditEntry `json:"items"`
	// NextCursor is passed back as the cursor parameter to get the next
	// (older) page. It is empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
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
	// CodeAgentUnavailable means skep-api could not reach the host agent.
	CodeAgentUnavailable = "agent_unavailable"
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
