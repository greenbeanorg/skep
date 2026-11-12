// Package api is the HTTP layer of the Skep REST API under /api/v1.
//
// Handlers translate between HTTP and the Backend interface and contain no
// business logic. skep-api serves this API; its Backend is the client for
// the agent's socket (internal/agentclient), so nothing here touches the
// host.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"

	"github.com/greenbeanorg/skep/internal/agentproto"
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

// Options configures the public API handler.
type Options struct {
	// Backend is the service layer, in skep-api the agent client.
	Backend Backend
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// TLS describes the certificate in use. It is reported in the system
	// document, and decides whether Strict-Transport-Security is sent.
	// Nil when the handler is not served over TLS (tests).
	TLS func() models.TLSInfo
}

type handler struct {
	backend Backend
	log     *slog.Logger
	tls     func() models.TLSInfo
}

// NewHandler returns the API as an http.Handler, wrapped in the request ID,
// access log, panic recovery, and security header middleware.
func NewHandler(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &handler{backend: opts.Backend, log: log, tls: opts.TLS}

	mux := http.NewServeMux()
	mux.Handle(Prefix+"/health", httpkit.ReadOnly(h.health))
	mux.Handle(Prefix+"/system", httpkit.ReadOnly(h.system))
	mux.Handle(Prefix+"/nodes/{id}/hardware", httpkit.ReadOnly(h.nodeHardware))
	mux.Handle(Prefix+"/tasks", httpkit.ReadOnly(h.tasks))
	mux.Handle(Prefix+"/tasks/{id}", httpkit.ReadOnly(h.task))
	mux.HandleFunc("/", httpkit.NotFound)

	var root http.Handler = mux
	root = withCaller(root)
	root = httpkit.WithRecovery(log, root)
	root = httpkit.WithAccessLog(log, root)
	root = h.withHSTS(root)
	root = httpkit.WithSecurityHeaders(root)
	root = httpkit.WithRequestID(root)
	return root
}

// hstsValue is two years, without includeSubDomains: Skep does not know
// what else lives under the host's domain.
const hstsValue = "max-age=63072000"

// withHSTS sends Strict-Transport-Security only while an operator-supplied
// certificate is in use. With the self-signed certificate it must not be
// sent: a browser that has stored the header refuses to let the user click
// through a certificate warning, which would lock people out of a fresh
// install (ADR 0006 decision 6).
func (h *handler) withHSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.tls != nil && h.tls().Source == TLSSourceOperator {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

// TLSSourceOperator is the TLSInfo.Source of an operator-supplied
// certificate.
const TLSSourceOperator = "operator"

// AnonymousActor is the actor of every request until the API has
// authentication. The API is loopback-only and read-only while it is used.
const AnonymousActor = "anonymous"

// withCaller attaches the request metadata the agent requires to the
// request context: the actor, the request ID, and the client's address.
func withCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := agentproto.Caller{
			ActorID:   AnonymousActor,
			ActorName: AnonymousActor,
			RequestID: httpkit.RequestID(r.Context()),
			SourceIP:  sourceIP(r),
		}
		next.ServeHTTP(w, r.WithContext(agentproto.WithCaller(r.Context(), caller)))
	})
}

// sourceIP is the address of the TCP peer. Forwarding headers are not
// trusted.
func sourceIP(r *http.Request) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return ap.Addr().Unmap().WithZone("").String()
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
	if h.tls != nil {
		sys.API = &models.APIInfo{TLS: h.tls()}
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
