// Package guestinv is skep-api's side of the guest inventory (ADR 0013
// amendment 6, step 8d-1): the writes it sends through skep-cluster's
// guests-write (reserve, release, add, remove), the read, and the
// reconciliation that brings the inventory in line with what this node's
// agent has.
package guestinv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/ids"
)

// Time limits of one write (D26, as usercopy's).
const (
	// RepeatFor is how long a write is repeated with its operation ID
	// while skep-cluster cannot commit it.
	RepeatFor   = 10 * time.Second
	repeatPause = 500 * time.Millisecond
)

// Outcomes of a write that could not be committed (the users' words).
const (
	OutcomeNotWritten = "not_written"
	OutcomeUnknown    = "unknown"
)

// Client is skep-cluster's socket as the inventory uses it
// (guestapi.Client).
type Client interface {
	Read(ctx context.Context) (guestapi.GuestsAnswer, error)
	Write(ctx context.Context, req guestapi.WriteRequest) (guestapi.WriteResult, error)
}

// Refused is a refusal of the state machine (D18): nothing was written.
type Refused struct {
	Code    string
	Message string
}

func (e *Refused) Error() string { return e.Code + ": " + e.Message }

// Uncommitted is a write skep-cluster could not commit within RepeatFor:
// Outcome says whether it is surely not written or unknown.
type Uncommitted struct {
	Outcome string
	Err     error
}

func (e *Uncommitted) Error() string {
	return "skep-cluster could not commit the guest entry (" + e.Outcome + "): " + e.Err.Error()
}

func (e *Uncommitted) Unwrap() error { return e.Err }

// Inventory sends guest writes and reads the inventory.
type Inventory struct {
	client Client
	log    *slog.Logger
	repeat time.Duration
	pause  time.Duration
}

// New returns the inventory on client.
func New(client Client, log *slog.Logger) *Inventory {
	if log == nil {
		log = slog.Default()
	}
	return &Inventory{client: client, log: log, repeat: RepeatFor, pause: repeatPause}
}

// List reads the inventory once.
func (i *Inventory) List(ctx context.Context) (guestapi.GuestsAnswer, error) {
	return i.client.Read(ctx)
}

// Reserve holds kind and name for this node under key.
func (i *Inventory) Reserve(ctx context.Context, kind, name, key, actor, requestID string) (guestapi.WriteResult, error) {
	return i.write(ctx, clusterstate.TypeGuestReserve, guestapi.ReserveData{Kind: kind, Name: name, Key: key}, actor, requestID)
}

// Release gives up this node's reservation of kind and name under key.
func (i *Inventory) Release(ctx context.Context, kind, name, key, actor, requestID string) (guestapi.WriteResult, error) {
	return i.write(ctx, clusterstate.TypeGuestRelease, guestapi.ReleaseData{Kind: kind, Name: name, Key: key}, actor, requestID)
}

// Add records a guest of this node.
func (i *Inventory) Add(ctx context.Context, id, kind, name, taskID, actor, requestID string) (guestapi.WriteResult, error) {
	return i.write(ctx, clusterstate.TypeGuestAdd, guestapi.AddData{ID: id, Kind: kind, Name: name, TaskID: taskID}, actor, requestID)
}

// Remove drops a guest of this node.
func (i *Inventory) Remove(ctx context.Context, id, actor, requestID string) (guestapi.WriteResult, error) {
	return i.write(ctx, clusterstate.TypeGuestRemove, guestapi.RemoveData{ID: id}, actor, requestID)
}

// write sends one entry with one operation ID, repeated with it for up to
// RepeatFor on the answers usercopy.Writer repeats on.
func (i *Inventory) write(ctx context.Context, typ string, data any, actor, requestID string) (guestapi.WriteResult, error) {
	op, err := ids.NewUUID()
	if err != nil {
		return guestapi.WriteResult{}, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return guestapi.WriteResult{}, err
	}
	req := guestapi.WriteRequest{Type: typ, OpID: op, Actor: actor, RequestID: requestID, Data: raw}
	deadline := time.Now().Add(i.repeat)
	unknown := false
	for {
		res, err := i.client.Write(ctx, req)
		if err == nil {
			return res, nil
		}
		transient, unk := isTransient(err)
		unknown = unknown || unk
		if !transient {
			var e *guestapi.Error
			if errors.As(err, &e) {
				return res, &Refused{Code: e.Code, Message: e.Message}
			}
			return res, &Uncommitted{Outcome: OutcomeUnknown, Err: err}
		}
		if time.Now().Add(i.pause).After(deadline) || ctx.Err() != nil {
			outcome := OutcomeNotWritten
			if unknown {
				outcome = OutcomeUnknown
			}
			return res, &Uncommitted{Outcome: outcome, Err: err}
		}
		select {
		case <-ctx.Done():
		case <-time.After(i.pause):
		}
	}
}

// isTransient reports whether a write may be repeated with the same
// operation ID, and whether its outcome is unknown: usercopy's rule.
func isTransient(err error) (transient, unknown bool) {
	var e *guestapi.Error
	switch {
	case errors.Is(err, guestapi.ErrUnreachable):
		return true, false
	case errors.Is(err, guestapi.ErrNoAnswer):
		return true, true
	case errors.As(err, &e):
		switch e.Code {
		case "no_leader", "unavailable", "not_committed", "removed", "joining", "forbidden":
			return true, false
		case "outcome_unknown":
			return true, true
		}
	}
	return false, false
}

// Service is the inventory with the reconciler's watch: what the API's
// create, clone and delete paths use.
type Service struct {
	*Inventory
	*Reconciler
}
