// Package api is the HTTP layer of the Skep REST API under /api/v1.
//
// Handlers translate between HTTP and the Backend interface and contain no
// business logic. In 0.1.0 the agent serves this API itself; because the
// package depends only on Backend, it can be mounted in skep-api later
// without changes.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Prefix is the path prefix of API version 1.
const Prefix = "/api/v1"

// Backend is what the handlers need from the service layer.
type Backend interface {
	Health(ctx context.Context) models.Health
	System(ctx context.Context) (models.System, error)
	Hardware(ctx context.Context, nodeID string) (models.Hardware, error)
	Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error)
	Task(ctx context.Context, id string) (models.TaskDetail, error)
}

// nodeIDRE is the accepted shape of a resource ID in a URL.
var nodeIDRE = regexp.MustCompile(httpkit.IDPattern)

type handler struct {
	backend Backend
	log     *slog.Logger
}

// NewHandler returns the API as an http.Handler, wrapped in the request ID,
// access log, panic recovery, and security header middleware.
func NewHandler(backend Backend, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &handler{backend: backend, log: log}

	mux := http.NewServeMux()
	mux.Handle(Prefix+"/health", httpkit.ReadOnly(h.health))
	mux.Handle(Prefix+"/system", httpkit.ReadOnly(h.system))
	mux.Handle(Prefix+"/nodes/{id}/hardware", httpkit.ReadOnly(h.nodeHardware))
	mux.Handle(Prefix+"/tasks", httpkit.ReadOnly(h.tasks))
	mux.Handle(Prefix+"/tasks/{id}", httpkit.ReadOnly(h.task))
	mux.HandleFunc("/", httpkit.NotFound)

	var root http.Handler = mux
	root = httpkit.WithRecovery(log, root)
	root = httpkit.WithAccessLog(log, root)
	root = httpkit.WithSecurityHeaders(root)
	root = httpkit.WithRequestID(root)
	return root
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	health := h.backend.Health(r.Context())
	status := http.StatusOK
	if health.Status != models.HealthOK {
		status = http.StatusServiceUnavailable
	}
	httpkit.WriteJSON(w, r, status, health)
}

func (h *handler) system(w http.ResponseWriter, r *http.Request) {
	sys, err := h.backend.System(r.Context())
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, sys)
}

func (h *handler) nodeHardware(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid node ID")
		return
	}
	hw, err := h.backend.Hardware(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, hw)
}

// tasks lists tasks, newest first. Tasks are read-only over the API in this
// release: nothing can be submitted or cancelled over the network until the
// API has authentication, because every task needs an actor.
func (h *handler) tasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := models.TaskListOptions{
		Status: models.TaskStatus(q.Get("status")),
		Cursor: q.Get("cursor"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.backend.Tasks(r.Context(), opts)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) task(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid task ID")
		return
	}
	detail, err := h.backend.Task(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, detail)
}
