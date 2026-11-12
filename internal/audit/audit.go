// Package audit defines the agent's operation audit entries.
//
// Every privileged operation the agent accepts or refuses is recorded in an
// append-only log in agent.db that skep-api cannot write to
// (docs/decisions/0004-privilege-boundary.md). This package holds the entry
// type and its rules; the SQLite writer lives in internal/database.
package audit

import (
	"context"
	"errors"
	"fmt"
)

// Outcome is the result recorded in an audit entry.
type Outcome string

// Outcomes.
const (
	// Success means the request was accepted, or the operation completed.
	Success Outcome = "success"
	// Failure means the request was invalid, or the operation did not
	// complete as requested.
	Failure Outcome = "failure"
	// Denied means the request was well-formed but refused by policy.
	Denied Outcome = "denied"
)

// Phase says which point of an operation an entry records.
type Phase string

// Phases.
const (
	// Requested is recorded when an operation is submitted.
	Requested Phase = "requested"
	// Finished is recorded when an operation reaches a terminal state.
	Finished Phase = "finished"
)

// System is the actor for operations the agent starts itself.
const System = "system"

// Entry is one audit record.
type Entry struct {
	// ActorID and ActorName identify who the operation was requested for,
	// as asserted by skep-api. Both are mandatory.
	ActorID   string
	ActorName string
	// PeerUID is the kernel-verified UID of the process that sent the
	// request. For operations the agent starts itself it is the agent's
	// own UID.
	PeerUID uint32
	// Action is the operation, for example "discovery.refresh".
	Action       string
	ResourceType string
	ResourceID   string
	TaskID       string
	Phase        Phase
	Outcome      Outcome
	// TaskStatus is the terminal task status of a Finished entry.
	TaskStatus string
	SourceIP   string
	RequestID  string
	// Details is optional structured context. It must not contain secrets.
	Details map[string]any
}

// ErrActorRequired is returned for an entry, or an operation, without an
// actor. Such an operation is refused before anything is written.
var ErrActorRequired = errors.New("audit: actor_id and actor_name are required")

// Validate checks the rules every entry must satisfy.
func (e Entry) Validate() error {
	if e.ActorID == "" || e.ActorName == "" {
		return ErrActorRequired
	}
	if e.Action == "" {
		return errors.New("audit: action is required")
	}
	switch e.Phase {
	case Requested, Finished:
	default:
		return fmt.Errorf("audit: invalid phase %q", e.Phase)
	}
	switch e.Outcome {
	case Success, Failure, Denied:
	default:
		return fmt.Errorf("audit: invalid outcome %q", e.Outcome)
	}
	return nil
}

// Writer appends entries to the audit log.
type Writer interface {
	Write(ctx context.Context, e Entry) error
}
