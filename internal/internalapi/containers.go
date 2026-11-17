package internalapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/containers"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// ContainerBackend is what the container handlers need from the agent's
// jail service (ADR 0010 decisions 8, 9, and 13).
type ContainerBackend interface {
	Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error)
	Container(ctx context.Context, id string) (models.Container, error)
	ConsoleLog(ctx context.Context, id string) (models.ContainerConsoleLog, error)
	CreateContainer(ctx context.Context, req models.ContainerCreate, sub containers.Submission) (models.ContainerCreated, error)
	UpdateContainer(ctx context.Context, id string, req models.ContainerUpdate, sub containers.Submission) (models.Task, error)
	DeleteContainer(ctx context.Context, id string, sub containers.Submission) (models.Task, error)
	Action(ctx context.Context, id string, act models.ContainerAction, sub containers.Submission) (models.Task, error)
}

func (h *handler) registerContainers(mux *http.ServeMux) {
	mux.Handle(agentproto.Prefix+"/containers", httpkit.Methods(h.containerCollection, http.MethodGet, http.MethodHead, http.MethodPost))
	mux.Handle(agentproto.Prefix+"/containers/{id}", httpkit.Methods(h.containerItem, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete))
	mux.Handle(agentproto.Prefix+"/containers/{id}/actions", httpkit.Methods(h.containerActions, http.MethodPost))
	mux.Handle(agentproto.Prefix+"/containers/{id}/console-log", httpkit.ReadOnly(h.containerConsoleLog))
}

// containerSubmission is the submission of a container write, from the
// headers the middleware already checked.
func containerSubmission(r *http.Request) containers.Submission {
	s := submission(r)
	return containers.Submission{Actor: s.Actor, PeerUID: s.PeerUID, RequestID: s.RequestID,
		SourceIP: s.SourceIP, IdempotencyKey: s.IdempotencyKey}
}

func (h *handler) containerCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req models.ContainerCreate
		if !decode(w, r, &req) {
			return
		}
		created, err := h.containers.CreateContainer(r.Context(), req, containerSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, created)
		return
	}
	q := r.URL.Query()
	opts := models.ContainerListOptions{Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.containers.Containers(r.Context(), opts)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

// containerID reads and checks the ID in the path.
func containerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid container ID")
		return "", false
	}
	return id, true
}

func (h *handler) containerItem(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPut:
		var upd models.ContainerUpdate
		if !decode(w, r, &upd) {
			return
		}
		task, err := h.containers.UpdateContainer(r.Context(), id, upd, containerSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
	case http.MethodDelete:
		task, err := h.containers.DeleteContainer(r.Context(), id, containerSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
	default:
		c, err := h.containers.Container(r.Context(), id)
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusOK, c)
	}
}

func (h *handler) containerActions(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var act models.ContainerAction
	if !decode(w, r, &act) {
		return
	}
	task, err := h.containers.Action(r.Context(), id, act, containerSubmission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) containerConsoleLog(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	log, err := h.containers.ConsoleLog(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, log)
}
