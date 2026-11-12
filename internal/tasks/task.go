// Package tasks is the agent's task engine: every operation the agent
// performs on the host runs as a task with a persistent state, an event
// history, and audit entries. See docs/decisions/0005-agent-task-engine.md.
//
// The engine never resumes or retries work on its own. Tasks that were not
// finished when the agent stopped are marked interrupted at the next start.
package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Error codes stored on tasks that did not succeed.
const (
	// CodeAgentRestart marks tasks found unfinished when the agent started.
	CodeAgentRestart = "agent_restart"
	// CodeAgentShutdown marks tasks stopped by a graceful shutdown.
	CodeAgentShutdown = "agent_shutdown"
	// CodeCancelled marks tasks that stopped because of a cancel request.
	CodeCancelled = "cancelled"
	// CodeTimeout marks tasks that exceeded their type's time limit.
	CodeTimeout = "timeout"
	// CodeFailed is the default code of a failed task.
	CodeFailed = "task_failed"
	// CodePanic marks tasks whose handler panicked.
	CodePanic = "internal_error"
)

// Task is the engine's full record of a task, including the fields that
// are not exposed through the API.
type Task struct {
	ID             string
	NodeID         string
	Type           string
	Status         models.TaskStatus
	ResourceType   string
	ResourceID     string
	IdempotencyKey string
	Actor          models.TaskActor
	// PeerUID is the kernel-verified UID of the submitter.
	PeerUID   uint32
	RequestID string
	// RequestHash fingerprints the request for idempotent replays.
	RequestHash       string
	Payload           json.RawMessage
	Cancellable       bool
	CancelRequestedAt *time.Time
	Result            json.RawMessage
	ErrorCode         string
	ErrorMessage      string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

// Public returns the API representation of the task.
func (t Task) Public() models.Task {
	p := models.Task{
		ID:                t.ID,
		NodeID:            t.NodeID,
		Type:              t.Type,
		Status:            t.Status,
		ResourceType:      t.ResourceType,
		ResourceID:        t.ResourceID,
		Actor:             t.Actor,
		RequestID:         t.RequestID,
		Cancellable:       t.Cancellable,
		CancelRequestedAt: t.CancelRequestedAt,
		CreatedAt:         t.CreatedAt,
		StartedAt:         t.StartedAt,
		FinishedAt:        t.FinishedAt,
	}
	if len(t.Result) > 0 {
		p.Result = t.Result
	}
	if t.ErrorCode != "" {
		p.Error = &models.TaskError{Code: t.ErrorCode, Message: t.ErrorMessage}
	}
	return p
}

// Request asks the engine to run a task.
type Request struct {
	// Type selects a registered Definition.
	Type string
	// ResourceType and ResourceID name the resource the task operates on.
	// Set both or neither. A resource has at most one unfinished task.
	ResourceType string
	ResourceID   string
	// Payload is the JSON input of the task. Empty means {}.
	Payload json.RawMessage
	// IdempotencyKey is optional. See Engine.Submit.
	IdempotencyKey string
	// Actor is who the task is requested for. It is mandatory.
	Actor models.TaskActor
	// PeerUID is the kernel-verified UID of the caller.
	PeerUID   uint32
	RequestID string
	SourceIP  string
}

// CancelRequest asks the engine to cancel a task.
type CancelRequest struct {
	// Actor is who asked for the cancellation. It is mandatory and may
	// differ from the actor who submitted the task.
	Actor     models.TaskActor
	PeerUID   uint32
	RequestID string
	SourceIP  string
}

// Reporter lets a running task record progress in its event history.
// Messages and data are readable through the API and must not contain
// secrets.
type Reporter interface {
	Progress(message string, data any)
}

// Handler performs a task. It must return promptly when ctx is cancelled,
// at a point where it is safe to stop, and return ctx.Err() in that case.
// The returned value is stored as the task's JSON result.
type Handler func(ctx context.Context, task Task, report Reporter) (result any, err error)

// Definition registers a task type. There is no generic "run this" task:
// every type is declared in code.
type Definition struct {
	// Type is the task type, for example "discovery.refresh".
	Type string
	// Cancellable says whether a running task of this type may be
	// cancelled. Cancelling a running task of a non-cancellable type is
	// refused. A pending task can always be cancelled: nothing has run.
	Cancellable bool
	// Timeout is the maximum run time. It is mandatory.
	Timeout time.Duration
	// Validate checks the payload. Nil accepts any JSON.
	Validate func(payload json.RawMessage) error
	// Run performs the task.
	Run Handler
}

// Failure is returned by a handler to fail a task with a specific code and
// a message written for an administrator. Any other error fails the task
// with a generic message, and the cause goes to the agent log only.
type Failure struct {
	Code    string
	Message string
}

// Error implements error.
func (f *Failure) Error() string { return f.Code + ": " + f.Message }

// Transition is one state change of a task. The store applies it only if
// the task is still in From, and writes the state event and the audit
// entries in the same transaction.
type Transition struct {
	From models.TaskStatus
	To   models.TaskStatus
	At   time.Time
	// Result is stored when To is succeeded.
	Result json.RawMessage
	// ErrorCode and ErrorMessage are stored for other terminal states.
	ErrorCode    string
	ErrorMessage string
	// Extra is written to the audit log with the transition, in addition
	// to the "finished" entry the store writes for a terminal state.
	Extra []audit.Entry
}

// Store persists tasks. The SQLite implementation is in internal/database.
//
// Every method that changes a task writes the change, its state event, and
// its audit entries atomically.
type Store interface {
	// Create stores a new pending task with its "requested" audit entry.
	// If the task's idempotency key already exists with the same request
	// hash, the existing task is returned and created is false. It returns
	// a models.Error with CodeIdempotencyConflict when the key exists with
	// a different hash, and with CodeResourceBusy when the resource already
	// has an unfinished task.
	Create(ctx context.Context, t Task, sourceIP string) (task Task, created bool, err error)
	// Transition applies tr. ok is false, without error, when the task was
	// not in tr.From.
	Transition(ctx context.Context, id string, tr Transition) (task Task, ok bool, err error)
	// RequestCancel records a cancel request on a running task. ok is
	// false when the task is not running.
	RequestCancel(ctx context.Context, id string, at time.Time, entry audit.Entry) (task Task, ok bool, err error)
	// InterruptAll marks every unfinished task interrupted and returns them.
	InterruptAll(ctx context.Context, at time.Time, code, message string) ([]Task, error)
	// AddProgress appends a progress event.
	AddProgress(ctx context.Context, id string, at time.Time, level, message string, data any) error
	// GetByKey returns the task created with an idempotency key, if any.
	GetByKey(ctx context.Context, key string) (task Task, found bool, err error)
	// Get returns a task, or models.ErrNotFound.
	Get(ctx context.Context, id string) (Task, error)
	// List returns a page of tasks, newest first, and the next cursor.
	List(ctx context.Context, opts models.TaskListOptions) ([]Task, string, error)
	// Events returns a task's events, oldest first.
	Events(ctx context.Context, id string) ([]models.TaskEvent, error)
}

var (
	typeRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// Limits on a request.
const (
	maxTypeLength = 64
	// MaxPayloadBytes bounds a task payload.
	MaxPayloadBytes = 256 << 10
)

// canonicalPayload validates the payload and returns it in canonical form:
// object keys sorted, no insignificant whitespace. Empty input means {}.
func canonicalPayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > MaxPayloadBytes {
		return nil, fmt.Errorf("payload is larger than %d bytes", MaxPayloadBytes)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("payload must be a JSON object")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("payload cannot be encoded")
	}
	return out, nil
}

// fingerprint identifies a request for idempotent replays. It covers the
// task type, the resource, the canonical payload, and the actor, so the
// same key sent by a different actor is a conflict, not a replay.
func fingerprint(taskType, resourceType, resourceID, actorID string, payload json.RawMessage) string {
	// A JSON array is an unambiguous encoding of the fields.
	b, _ := json.Marshal([]string{taskType, resourceType, resourceID, actorID, string(payload)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
