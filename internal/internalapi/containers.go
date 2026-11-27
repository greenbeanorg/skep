package internalapi

import (
	"context"
	"net/http"
	"strconv"

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
	// Snapshots and clones (decisions 2 and 3).
	Snapshots(ctx context.Context, id string) (models.SnapshotList, error)
	Snapshot(ctx context.Context, id, snapID string) (models.Snapshot, error)
	CreateSnapshot(ctx context.Context, id string, req models.SnapshotCreate, sub containers.Submission) (models.SnapshotCreated, error)
	RollbackSnapshot(ctx context.Context, id, snapID string, req models.SnapshotRollback, sub containers.Submission) (models.Task, error)
	DeleteSnapshot(ctx context.Context, id, snapID string, sub containers.Submission) (models.Task, error)
	CloneContainer(ctx context.Context, id string, req models.ContainerClone, sub containers.Submission) (models.ContainerCreated, error)
}

func (h *handler) registerContainers(add func(string, http.HandlerFunc)) {
	add("/containers", h.containerCollection)
	add("/containers/{id}", h.containerItem)
	add("/containers/{id}/actions", h.containerActions)
	add("/containers/{id}/console-log", h.containerConsoleLog)
	add("/containers/{id}/snapshots", h.containerSnapshotCollection)
	add("/containers/{id}/snapshots/{sid}", h.containerSnapshotItem)
	add("/containers/{id}/snapshots/{sid}/rollback", h.containerSnapshotRollback)
	add("/containers/{id}/clone", h.containerClone)
}

// containerSubmission is the submission of a container write, from the
// headers the middleware already checked.
func containerSubmission(r *http.Request) containers.Submission {
	s := submission(r)
	return containers.Submission{Actor: s.Actor, PeerUID: s.PeerUID, RequestID: s.RequestID,
		SourceIP: s.SourceIP, IdempotencyKey: s.IdempotencyKey, OriginNode: s.OriginNode, Envelope: s.Envelope}
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

// containerSnapshotIDs reads and checks the IDs of a jail snapshot route.
func containerSnapshotIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	id, ok := containerID(w, r)
	if !ok {
		return "", "", false
	}
	snapID := r.PathValue("sid")
	if !idRE.MatchString(snapID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid snapshot ID")
		return "", "", false
	}
	return id, snapID, true
}

func (h *handler) containerSnapshotCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var req models.SnapshotCreate
		if !decode(w, r, &req) {
			return
		}
		created, err := h.containers.CreateSnapshot(r.Context(), id, req, containerSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, created)
		return
	}
	list, err := h.containers.Snapshots(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) containerSnapshotItem(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := containerSnapshotIDs(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		task, err := h.containers.DeleteSnapshot(r.Context(), id, snapID, containerSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
		return
	}
	snap, err := h.containers.Snapshot(r.Context(), id, snapID)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, snap)
}

func (h *handler) containerSnapshotRollback(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := containerSnapshotIDs(w, r)
	if !ok {
		return
	}
	var req models.SnapshotRollback
	if !decode(w, r, &req) {
		return
	}
	task, err := h.containers.RollbackSnapshot(r.Context(), id, snapID, req, containerSubmission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) containerClone(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var req models.ContainerClone
	if !decode(w, r, &req) {
		return
	}
	created, err := h.containers.CloneContainer(r.Context(), id, req, containerSubmission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}
