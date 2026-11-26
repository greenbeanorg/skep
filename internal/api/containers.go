package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Jails (ADR 0010 decisions 8, 9, 13, and 14). Reading is container:read;
// creating and updating container:write; deleting container:delete;
// start, stop, and restart container:power; snapshots container:snapshot,
// and a clone container:write and container:snapshot (amendment j). Every
// write is audited in the access log with the container as the resource.

// ContainerBackend is what the container handlers need from the service
// layer: in skep-api, the agent client.
type ContainerBackend interface {
	Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error)
	Container(ctx context.Context, id string) (models.Container, error)
	ContainerConsoleLog(ctx context.Context, id string) (models.ContainerConsoleLog, error)
	CreateContainer(ctx context.Context, req models.ContainerCreate) (models.ContainerCreated, error)
	UpdateContainer(ctx context.Context, id string, upd models.ContainerUpdate) (models.Task, error)
	DeleteContainer(ctx context.Context, id string) (models.Task, error)
	ContainerAction(ctx context.Context, id string, act models.ContainerAction) (models.Task, error)
	ContainerSnapshots(ctx context.Context, id string) (models.SnapshotList, error)
	ContainerSnapshot(ctx context.Context, id, snapID string) (models.Snapshot, error)
	CreateContainerSnapshot(ctx context.Context, id string, req models.SnapshotCreate) (models.SnapshotCreated, error)
	RollbackContainerSnapshot(ctx context.Context, id, snapID string, req models.SnapshotRollback) (models.Task, error)
	DeleteContainerSnapshot(ctx context.Context, id, snapID string) (models.Task, error)
	CloneContainer(ctx context.Context, id string, req models.ContainerClone) (models.ContainerCreated, error)
}

func (h *handler) containerRoutes() []route {
	return []route{
		{http.MethodGet, "/containers", auth.PermContainerRead, nil, h.listContainers, servedWithoutQuorum},
		{http.MethodPost, "/containers", auth.PermContainerWrite, nil, h.createContainer, needsQuorum},
		{http.MethodGet, "/containers/{id}", auth.PermContainerRead, nil, h.getContainer, servedWithoutQuorum},
		{http.MethodPut, "/containers/{id}", auth.PermContainerWrite, nil, h.updateContainer, needsQuorum},
		{http.MethodDelete, "/containers/{id}", auth.PermContainerDelete, nil, h.deleteContainer, needsQuorum},
		{http.MethodPost, "/containers/{id}/actions", auth.PermContainerPower, nil, h.containerAction, servedWithoutQuorum},
		{http.MethodGet, "/containers/{id}/console-log", auth.PermContainerRead, nil, h.containerConsoleLog, servedWithoutQuorum},
		{http.MethodGet, "/containers/{id}/snapshots", auth.PermContainerRead, nil, h.listContainerSnapshots, servedWithoutQuorum},
		{http.MethodPost, "/containers/{id}/snapshots", auth.PermContainerSnapshot, nil, h.createContainerSnapshot, needsQuorum},
		{http.MethodGet, "/containers/{id}/snapshots/{sid}", auth.PermContainerRead, nil, h.getContainerSnapshot, servedWithoutQuorum},
		{http.MethodDelete, "/containers/{id}/snapshots/{sid}", auth.PermContainerSnapshot, nil, h.deleteContainerSnapshot, needsQuorum},
		// A rollback destroys the jail's current data (and, with
		// destroy_newer, newer snapshots): container:write as well as
		// container:snapshot, the same as a clone (amendment n, SR-11).
		{http.MethodPost, "/containers/{id}/snapshots/{sid}/rollback", auth.PermContainerWrite, []string{auth.PermContainerSnapshot}, h.rollbackContainerSnapshot, needsQuorum},
		{http.MethodPost, "/containers/{id}/clone", auth.PermContainerWrite, []string{auth.PermContainerSnapshot}, h.cloneContainer, needsQuorum},
	}
}

func containerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid container ID")
		return "", false
	}
	return id, true
}

// auditContainer records a container write in the access log.
func (h *handler) auditContainer(r *http.Request, action, id string, task *models.Task, err error) {
	who, _ := identityFrom(r.Context())
	entry := audit.AccessEntry{
		ActorID: who.User.ID, ActorName: who.User.Username, Action: action,
		ResourceType: models.ContainerResource, ResourceID: id, Outcome: audit.Success,
	}
	if err == nil {
		if task != nil {
			entry.Details = map[string]any{"task_id": task.ID}
		}
	} else {
		entry.Outcome = audit.Failure
		var me *models.Error
		switch {
		case errors.As(err, &me):
			entry.Details = map[string]any{"error_code": me.Code}
			if me.Code != models.CodeInvalidRequest && me.Code != models.CodeAgentUnavailable &&
				me.Code != models.CodeQueueFull && me.Code != models.CodeShuttingDown && me.Code != models.CodeNodeDraining &&
				me.Code != auth.CodeNoQuorum {
				entry.Outcome = audit.Denied
			}
		case errors.Is(err, models.ErrNotFound):
			entry.Details = map[string]any{"error_code": httpkit.CodeNotFound}
		}
	}
	h.writeAudit(r, entry)
}

func (h *handler) listContainers(w http.ResponseWriter, r *http.Request) {
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
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getContainer(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	c, err := h.containers.Container(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, c)
}

func (h *handler) containerConsoleLog(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	log, err := h.containers.ContainerConsoleLog(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, log)
}

func (h *handler) createContainer(w http.ResponseWriter, r *http.Request) {
	var req models.ContainerCreate
	if !decodeBody(w, r, &req) {
		return
	}
	var ok bool
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	r, key, ok := h.reserveGuest(w, r, clusterstate.KindContainer, req.Name, func(r *http.Request, err error) {
		h.auditContainer(r, models.TaskContainerCreate, "", nil, err)
	})
	if !ok {
		return
	}
	created, err := h.containers.CreateContainer(r.Context(), req)
	h.settleGuest(r, clusterstate.KindContainer, req.Name, key, created.Container.ID, created.Task, err)
	h.auditContainer(r, models.TaskContainerCreate, created.Container.ID, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/containers/"+created.Container.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

func (h *handler) updateContainer(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var upd models.ContainerUpdate
	if !decodeBody(w, r, &upd) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.containers.UpdateContainer(r.Context(), id, upd)
	h.auditContainer(r, models.TaskContainerUpdate, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) deleteContainer(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.containers.DeleteContainer(r.Context(), id)
	h.auditContainer(r, models.TaskContainerDelete, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.watchGuestTask(task)
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

// containerActionNames maps an action to the audit action of its task.
var containerActionNames = map[string]string{
	models.ContainerActionStart:   models.TaskContainerStart,
	models.ContainerActionStop:    models.TaskContainerStop,
	models.ContainerActionRestart: models.TaskContainerRestart,
}

func (h *handler) containerAction(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var act models.ContainerAction
	if !decodeBody(w, r, &act) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	action, known := containerActionNames[act.Action]
	if !known {
		action = "container.action"
	}
	task, err := h.containers.ContainerAction(r.Context(), id, act)
	h.auditContainer(r, action, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func containerSnapshotIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	id, ok := containerID(w, r)
	if !ok {
		return "", "", false
	}
	snapID := r.PathValue("sid")
	if !nodeIDRE.MatchString(snapID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid snapshot ID")
		return "", "", false
	}
	return id, snapID, true
}

func (h *handler) listContainerSnapshots(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	list, err := h.containers.ContainerSnapshots(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getContainerSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := containerSnapshotIDs(w, r)
	if !ok {
		return
	}
	snap, err := h.containers.ContainerSnapshot(r.Context(), id, snapID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, snap)
}

func (h *handler) createContainerSnapshot(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var req models.SnapshotCreate
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	created, err := h.containers.CreateContainerSnapshot(r.Context(), id, req)
	h.auditContainer(r, models.TaskContainerSnapshotCreate, id, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/containers/"+id+"/snapshots/"+created.Snapshot.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

func (h *handler) rollbackContainerSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := containerSnapshotIDs(w, r)
	if !ok {
		return
	}
	var req models.SnapshotRollback
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.containers.RollbackContainerSnapshot(r.Context(), id, snapID, req)
	h.auditContainer(r, models.TaskContainerSnapshotRollback, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) deleteContainerSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := containerSnapshotIDs(w, r)
	if !ok {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.containers.DeleteContainerSnapshot(r.Context(), id, snapID)
	h.auditContainer(r, models.TaskContainerSnapshotDelete, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

// cloneContainer asks for a linked clone. The answer is 202 with the
// clone, not yet provisioned, as for a create.
func (h *handler) cloneContainer(w http.ResponseWriter, r *http.Request) {
	id, ok := containerID(w, r)
	if !ok {
		return
	}
	var req models.ContainerClone
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	r, key, ok := h.reserveGuest(w, r, clusterstate.KindContainer, req.Name, func(r *http.Request, err error) {
		h.auditContainer(r, models.TaskContainerClone, id, nil, err)
	})
	if !ok {
		return
	}
	created, err := h.containers.CloneContainer(r.Context(), id, req)
	h.settleGuest(r, clusterstate.KindContainer, req.Name, key, created.Container.ID, created.Task, err)
	h.auditContainer(r, models.TaskContainerClone, id, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/containers/"+created.Container.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

// noContainers answers every container operation as a host without jails
// would. It stands in when the handler has no container backend.
type noContainers struct{}

func (noContainers) Containers(context.Context, models.ContainerListOptions) (models.ContainerList, error) {
	return models.ContainerList{Items: []models.Container{}}, nil
}
func (noContainers) Container(context.Context, string) (models.Container, error) {
	return models.Container{}, models.ErrNotFound
}
func (noContainers) ContainerConsoleLog(context.Context, string) (models.ContainerConsoleLog, error) {
	return models.ContainerConsoleLog{}, models.ErrNotFound
}
func (noContainers) CreateContainer(context.Context, models.ContainerCreate) (models.ContainerCreated, error) {
	return models.ContainerCreated{}, &models.Error{Code: models.CodeStorageFeatureUnsupported, Message: "jails are not available"}
}
func (noContainers) UpdateContainer(context.Context, string, models.ContainerUpdate) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noContainers) DeleteContainer(context.Context, string) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noContainers) ContainerAction(context.Context, string, models.ContainerAction) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noContainers) ContainerSnapshots(context.Context, string) (models.SnapshotList, error) {
	return models.SnapshotList{}, models.ErrNotFound
}
func (noContainers) ContainerSnapshot(context.Context, string, string) (models.Snapshot, error) {
	return models.Snapshot{}, models.ErrNotFound
}
func (noContainers) CreateContainerSnapshot(context.Context, string, models.SnapshotCreate) (models.SnapshotCreated, error) {
	return models.SnapshotCreated{}, models.ErrNotFound
}
func (noContainers) RollbackContainerSnapshot(context.Context, string, string, models.SnapshotRollback) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noContainers) DeleteContainerSnapshot(context.Context, string, string) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noContainers) CloneContainer(context.Context, string, models.ContainerClone) (models.ContainerCreated, error) {
	return models.ContainerCreated{}, models.ErrNotFound
}
