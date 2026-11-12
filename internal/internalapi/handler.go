// Package internalapi is the agent's side of the internal API: HTTP and
// JSON under /internal/v1 on a Unix domain socket, for skep-api only
// (ADR 0004, ADR 0006 decision 1).
//
// Two checks happen before any handler runs. The listener reads the peer's
// credentials from the kernel and drops everyone who is not on the
// allow-list. The handler then requires the actor headers on every request,
// reads included. The agent records the actor; it makes no authorization
// decision from it.
package internalapi

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// MaxRequestBody bounds the body of a request to the agent.
const MaxRequestBody = 1 << 20

// Backend is what the handlers need from the agent's service layer.
type Backend interface {
	Health(ctx context.Context) models.Health
	System(ctx context.Context) (models.System, error)
	Hardware(ctx context.Context, nodeID string) (models.Hardware, error)
	Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error)
	Task(ctx context.Context, id string) (models.TaskDetail, error)
}

// Options configures the handler.
type Options struct {
	// RequestTimeout bounds the time spent on one request.
	RequestTimeout time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

var idRE = regexp.MustCompile(httpkit.IDPattern)

type handler struct {
	backend Backend
	log     *slog.Logger
}

// NewHandler returns the internal API as an http.Handler. It must be served
// by an http.Server whose ConnContext is ConnContext and whose listener is
// a Listener; a request that did not arrive that way is refused.
func NewHandler(backend Backend, opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &handler{backend: backend, log: log}

	mux := http.NewServeMux()
	mux.Handle(agentproto.Prefix+"/health", httpkit.ReadOnly(h.health))
	mux.Handle(agentproto.Prefix+"/system", httpkit.ReadOnly(h.system))
	mux.Handle(agentproto.Prefix+"/nodes/{id}/hardware", httpkit.ReadOnly(h.nodeHardware))
	mux.Handle(agentproto.Prefix+"/tasks", httpkit.ReadOnly(h.tasks))
	mux.Handle(agentproto.Prefix+"/tasks/{id}", httpkit.ReadOnly(h.task))
	mux.HandleFunc("/", httpkit.NotFound)

	var root http.Handler = mux
	root = withCaller(root)
	root = withPeer(root)
	root = withLimits(opts.RequestTimeout, root)
	root = httpkit.WithRecovery(log, root)
	root = withRequestLog(log, root)
	root = withRequestID(root)
	return root
}

// withRequestID uses the request ID skep-api sent, so both logs and both
// audit trails name the same request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get(agentproto.HeaderRequestID); id != "" {
			r.Header.Set(httpkit.RequestIDHeader, id)
		}
		httpkit.WithRequestID(next).ServeHTTP(w, r)
	})
}

// withRequestLog logs one line per request at debug level: skep-api already
// writes the access log, and health checks would otherwise fill this one.
func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		peer, _ := PeerFrom(r.Context())
		log.LogAttrs(r.Context(), slog.LevelDebug, "internal request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Duration("duration", time.Since(start)),
			slog.Uint64("peer_uid", uint64(peer.UID)),
			slog.String("actor_id", r.Header.Get(agentproto.HeaderActorID)),
			slog.String("request_id", httpkit.RequestID(r.Context())),
		)
	})
}

// withLimits bounds the body size and the time of every request.
func withLimits(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
		if timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// withPeer refuses a request whose connection has no verified peer. With
// the agent's own listener that cannot happen; it guards against the
// handler being mounted on anything else.
func withPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PeerFrom(r.Context()); !ok {
			httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "the peer of this connection is not verified")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CodeForbidden is the error code for a request without a verified peer.
const CodeForbidden = "forbidden"

// withCaller refuses any request without valid actor headers, before a
// handler runs, and puts the caller in the request context.
func withCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := agentproto.FromHeaders(r.Header)
		if err != nil {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(agentproto.WithCaller(r.Context(), caller)))
	})
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	// Always 200: the document says whether the agent is degraded, and the
	// transport status says only that the agent answered.
	httpkit.WriteJSON(w, r, http.StatusOK, h.backend.Health(r.Context()))
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
	if !idRE.MatchString(id) {
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
	if !idRE.MatchString(id) {
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
