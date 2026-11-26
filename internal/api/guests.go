package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/guestinv"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// GuestInventory is the replicated guest inventory as the create and
// clone paths use it (step 8d-1, D26–D30): guestinv.Inventory with the
// reconciler's watch.
type GuestInventory interface {
	Reserve(ctx context.Context, kind, name, key, actor, requestID string) (guestapi.WriteResult, error)
	Release(ctx context.Context, kind, name, key, actor, requestID string) (guestapi.WriteResult, error)
	Add(ctx context.Context, id, kind, name, taskID, actor, requestID string) (guestapi.WriteResult, error)
	List(ctx context.Context) (guestapi.GuestsAnswer, error)
	// Watch makes reconciliation rounds while the task is unfinished.
	Watch(taskID string)
}

// guestKey is D27 (1): the client's Idempotency-Key, or a new UUID set on
// the agent call. It returns the request carrying the key.
func guestKey(r *http.Request) (*http.Request, string, error) {
	caller, _ := agentproto.CallerFrom(r.Context())
	if caller.IdempotencyKey != "" {
		return r, caller.IdempotencyKey, nil
	}
	key, err := ids.NewUUID()
	if err != nil {
		return r, "", err
	}
	caller.IdempotencyKey = key
	return r.WithContext(agentproto.WithCaller(r.Context(), caller)), key, nil
}

// guestActor is the actor and request ID of the inventory writes of a
// request.
func guestActor(r *http.Request) (string, string) {
	who, _ := identityFrom(r.Context())
	return "user:" + who.User.Username, httpkit.RequestID(r.Context())
}

// reserveGuest is D27 (1) and (2): the key, then the name's reservation.
// A name that fails the agents' rule is refused first, with nothing
// written or sent (F7 of the 8d-1 review). A failed reserve answers the
// error (D28) and the agent is not called. It returns the request to call
// the agent with, and the key.
func (h *handler) reserveGuest(w http.ResponseWriter, r *http.Request, kind, name string, audit func(*http.Request, error)) (*http.Request, string, bool) {
	if h.guests == nil {
		return r, "", true
	}
	if !models.ValidGuestName(name) {
		me := models.Invalid(models.GuestNameRule)
		audit(r, me)
		httpkit.WriteError(w, r, http.StatusBadRequest, me.Code, me.Message)
		return r, "", false
	}
	r, key, err := guestKey(r)
	if err != nil {
		h.fail(w, r, err)
		return r, "", false
	}
	actor, rid := guestActor(r)
	if _, err := h.guests.Reserve(r.Context(), kind, name, key, actor, rid); err != nil {
		status, me := guestFailure(kind, err)
		audit(r, me)
		httpkit.WriteErrorDetails(w, r, status, me.Code, me.Message, me.Details)
		return r, "", false
	}
	return r, key, true
}

// guestFailure maps a failed inventory write to the public error (D28).
func guestFailure(kind string, err error) (int, *models.Error) {
	var refused *guestinv.Refused
	var unc *guestinv.Uncommitted
	switch {
	case errors.As(err, &refused):
		msg := clean(refused.Message)
		switch refused.Code {
		case clusterstate.CodeNameTaken:
			code := models.CodeVMNameTaken
			if kind == clusterstate.KindContainer {
				code = models.CodeContainerNameTaken
			}
			return http.StatusConflict, &models.Error{Code: code, Message: "the name is taken in the cluster: " + msg}
		case clusterstate.CodeTooManyGuests:
			return http.StatusConflict, &models.Error{Code: models.CodeTooManyGuests, Message: msg}
		case clusterstate.CodeInvalid, "invalid_request":
			return http.StatusBadRequest, &models.Error{Code: httpkit.CodeInvalidRequest, Message: msg}
		}
		return http.StatusConflict, &models.Error{Code: CodeClusterRefused, Message: "the cluster refused (" + refused.Code + "): " + msg}
	case errors.As(err, &unc):
		return http.StatusServiceUnavailable, auth.NoQuorum(unc.Outcome, "the name could not be reserved in the cluster: "+clean(unc.Err.Error())+"; the agent was not asked")
	}
	return http.StatusServiceUnavailable, auth.NoQuorum(auth.OutcomeUnknown, "the name could not be reserved in the cluster; the agent was not asked")
}

// agentRefused reports whether the agent's error is D27 (4b): the agent
// answered a refusal, or nothing was sent. Anything else (no answer, a
// timeout, another build, an internal error) is (4c).
func agentRefused(err error) bool {
	if errors.Is(err, agentclient.ErrNotSent) || errors.Is(err, models.ErrNotFound) {
		return true
	}
	var me *models.Error
	if !errors.As(err, &me) {
		return false
	}
	return me.Code != models.CodeAgentUnavailable && me.Code != models.CodeAgentVersionMismatch
}

// settleGuest is D27 (4a)–(4c), after the agent's answer. A failed write
// is logged; the answer stays the agent's, and reconciliation settles
// what is left.
func (h *handler) settleGuest(r *http.Request, kind, name, key, id string, task models.Task, agentErr error) {
	if h.guests == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	actor, rid := guestActor(r)
	log := func(what string, err error) {
		if err != nil {
			h.log.WarnContext(ctx, "guest inventory: "+what+" failed; reconciliation settles it", "kind", kind, "name", name, "guest_id", id, "error", err)
		}
	}
	switch {
	case agentErr == nil:
		// guest.add carries the name the agent reports for the guest it
		// has just read, not the request's (F3 of the 8d-1 review).
		var got string
		var err error
		if kind == clusterstate.KindVM {
			var v models.VM
			v, err = h.vms.VM(ctx, id)
			got = v.Name
		} else {
			var c models.Container
			c, err = h.containers.Container(ctx, id)
			got = c.Name
		}
		switch {
		case err == nil:
			_, werr := h.guests.Add(ctx, id, kind, got, task.ID, actor, rid)
			log("guest.add", werr)
			if got != name {
				h.log.WarnContext(ctx, "guest inventory: the agent reports another name than the request's; the reservation of the requested name is released",
					"kind", kind, "name", name, "agent_name", got, "guest_id", id)
				_, werr := h.guests.Release(ctx, kind, name, key, actor, rid)
				log("guest.release", werr)
			}
		case errors.Is(err, models.ErrNotFound):
			_, werr := h.guests.Release(ctx, kind, name, key, actor, rid)
			log("guest.release", werr)
		}
		h.guests.Watch(task.ID)
	case agentRefused(agentErr):
		_, werr := h.guests.Release(ctx, kind, name, key, actor, rid)
		log("guest.release", werr)
	}
}

// clusterGuests is GET /api/v1/cluster/guests (D34): the inventory, its
// reservations as "creating", every text from the socket cleaned.
func (h *handler) clusterGuests(w http.ResponseWriter, r *http.Request) {
	if h.guests == nil {
		e := auth.NoQuorum(auth.OutcomeNotWritten, "this node has no guest inventory")
		httpkit.WriteErrorDetails(w, r, http.StatusServiceUnavailable, e.Code, e.Message, e.Details)
		return
	}
	a, err := h.guests.List(r.Context())
	if err != nil {
		e := auth.NoQuorum(auth.OutcomeNotWritten, "skep-cluster's guest inventory cannot be read")
		httpkit.WriteErrorDetails(w, r, http.StatusServiceUnavailable, e.Code, e.Message, e.Details)
		return
	}
	out := models.ClusterGuestList{ClusterID: clean(a.ClusterID), GuestsIndex: a.GuestsIndex, Items: []models.ClusterGuest{}}
	for _, g := range a.Guests {
		out.Items = append(out.Items, models.ClusterGuest{ID: clean(g.ID), Name: clean(g.Name), Kind: clean(g.Kind),
			Owner: models.ClusterGuestOwner{NodeID: clean(g.Owner), NodeName: clean(g.OwnerName)}, State: models.GuestPresent,
			Flagged: g.Flagged, TaskID: clean(g.TaskID), Time: time.Unix(g.At, 0).UTC()})
	}
	for _, x := range a.Reservations {
		out.Items = append(out.Items, models.ClusterGuest{Name: clean(x.Name), Kind: clean(x.Kind),
			Owner: models.ClusterGuestOwner{NodeID: clean(x.Owner), NodeName: clean(x.OwnerName)}, State: models.GuestCreating,
			Time: time.Unix(x.At, 0).UTC()})
	}
	httpkit.WriteJSON(w, r, http.StatusOK, out)
}

// watchGuestTask makes reconciliation rounds while an accepted delete is
// unfinished (D29, D30): the entry goes when the guest is gone.
func (h *handler) watchGuestTask(task models.Task) {
	if h.guests != nil {
		h.guests.Watch(task.ID)
	}
}
