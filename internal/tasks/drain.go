package tasks

import (
	"context"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The drain (ADR 0013 amendment 6, step 8d-1 D3–D5): for a short time the
// engine refuses every new task, so that a caller with root on this host
// (skep-setup upgrade, skep-cluster join) can wait for the unfinished ones
// and act on a quiet node. It lives in the engine's memory only: an agent
// restart clears it, and it expires by the engine's clock unless renewed.

// Drain reasons. "maintenance" is not one (step 8f).
const (
	DrainUpgrade = "upgrade"
	DrainJoin    = "join"
)

// DrainFor is how long a drain lasts after it is set or renewed.
const DrainFor = 60 * time.Second

// The drain's audit actions (D5).
const (
	ActionDrainSet     = "node.drain.set"
	ActionDrainCleared = "node.drain.cleared"
	ActionDrainExpired = "node.drain.expired"
)

// Drain is an active drain.
type Drain struct {
	Reason    string
	ExpiresAt time.Time
}

// DrainActor is who set or cleared a drain: the actor named in the
// request's headers and the kernel-verified peer UID.
type DrainActor struct {
	Actor     models.TaskActor
	PeerUID   uint32
	RequestID string
}

// ValidDrainReason reports whether reason may be set.
func ValidDrainReason(reason string) bool { return reason == DrainUpgrade || reason == DrainJoin }

// SetDrain sets a drain with reason for DrainFor from now, or renews the
// drain of the same reason. Another reason while a drain is active is
// refused with drain_active and changes nothing. It returns the drain and
// the number of unfinished tasks.
func (e *Engine) SetDrain(ctx context.Context, reason string, by DrainActor) (Drain, int, error) {
	if !ValidDrainReason(reason) {
		return Drain{}, 0, models.Invalid(`reason must be "upgrade" or "join"`)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, active := e.activeDrainLocked(ctx)
	if active && cur.Reason != reason {
		return Drain{}, 0, &models.Error{Code: models.CodeDrainActive,
			Message: "a drain with another reason is active; nothing was changed",
			Details: map[string]any{"reason": cur.Reason, "expires_at": cur.ExpiresAt}}
	}
	d := Drain{Reason: reason, ExpiresAt: e.now().UTC().Add(DrainFor)}
	e.drain = &d
	if !active {
		e.auditDrainLocked(ctx, ActionDrainSet, by, d)
		e.log.Info("drain set: no new task is accepted", "reason", reason, "expires_at", d.ExpiresAt, "peer_uid", by.PeerUID)
	}
	return d, e.unfinishedLocked(), nil
}

// ClearDrain clears the drain. Without one it changes nothing.
func (e *Engine) ClearDrain(ctx context.Context, by DrainActor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, active := e.activeDrainLocked(ctx)
	if !active {
		return
	}
	e.drain = nil
	e.auditDrainLocked(ctx, ActionDrainCleared, by, d)
	e.log.Info("drain cleared", "reason", d.Reason, "peer_uid", by.PeerUID)
}

// CurrentDrain returns the active drain, if any, and the number of
// unfinished tasks.
func (e *Engine) CurrentDrain(ctx context.Context) (Drain, bool, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.activeDrainLocked(ctx)
	return d, ok, e.unfinishedLocked()
}

// activeDrainLocked returns the active drain. The first call that finds
// it expired clears it and records the expiry. e.mu is held.
func (e *Engine) activeDrainLocked(ctx context.Context) (Drain, bool) {
	if e.drain == nil {
		return Drain{}, false
	}
	if e.now().Before(e.drain.ExpiresAt) {
		return *e.drain, true
	}
	d := *e.drain
	e.drain = nil
	e.auditDrainLocked(ctx, ActionDrainExpired, DrainActor{Actor: models.TaskActor{ID: audit.System, Name: audit.System}}, d)
	e.log.Info("drain expired", "reason", d.Reason, "expired_at", d.ExpiresAt)
	return Drain{}, false
}

// unfinishedLocked counts the tasks accepted and not yet terminal,
// including one a worker has taken off the queue and not yet started (F6
// of the 8d-1 review). e.mu is held.
func (e *Engine) unfinishedLocked() int { return len(e.unfinished) }

func (e *Engine) auditDrainLocked(ctx context.Context, action string, by DrainActor, d Drain) {
	entry := audit.Entry{ActorID: by.Actor.ID, ActorName: by.Actor.Name, PeerUID: by.PeerUID, Action: action,
		ResourceType: "node", ResourceID: e.nodeID, Phase: audit.Finished, Outcome: audit.Success, RequestID: by.RequestID,
		Details: map[string]any{"reason": d.Reason, "expires_at": d.ExpiresAt}}
	if err := e.audit.Write(context.WithoutCancel(ctx), entry); err != nil {
		e.log.Error("tasks: write the drain's audit entry", "action", action, "error", err)
	}
}

// drainRefusal is Submit's answer while a drain is active.
func drainRefusal(d Drain) *models.Error {
	return &models.Error{Code: models.CodeNodeDraining,
		Message: "this node is draining (" + d.Reason + "): no new task is accepted until " + d.ExpiresAt.Format(time.RFC3339),
		Details: map[string]any{"reason": d.Reason, "expires_at": d.ExpiresAt}}
}
