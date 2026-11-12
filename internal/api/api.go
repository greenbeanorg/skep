// Package api is the HTTP layer of the Skep REST API under /api/v1.
//
// Handlers translate between HTTP and the service layer and contain no
// business logic. skep-api serves this API. Its Backend is the client for
// the agent's socket (internal/agentclient), so nothing here touches the
// host; identity comes from internal/auth.
//
// Every route is declared in one table together with what it requires.
// A route that is not marked public needs a session.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Prefix is the path prefix of API version 1.
const Prefix = "/api/v1"

// Backend is what the node handlers need from the service layer.
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
	// Auth checks sessions and performs logins.
	Auth *auth.Authenticator
	// Users manages accounts.
	Users *auth.Users
	// Audit is the access audit log.
	Audit auth.AuditWriter
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// TLS describes the certificate in use. It is reported in the system
	// document, and decides whether Strict-Transport-Security is sent.
	// Nil when the handler is not served over TLS (tests).
	TLS func() models.TLSInfo
	// TrustedProxies lists the proxies whose X-Forwarded-For header is
	// believed. Empty, the default, means the header is ignored and the
	// source address is always the TCP peer.
	TrustedProxies []netip.Prefix
}

type handler struct {
	backend Backend
	auth    *auth.Authenticator
	users   *auth.Users
	audit   auth.AuditWriter
	log     *slog.Logger
	tls     func() models.TLSInfo
	proxies []netip.Prefix
}

// access says what a route requires.
type access int

const (
	// public routes need no session. There are two: health and login.
	public access = iota
	// authenticated routes need a valid session.
	authenticated
)

// route is one entry of the route table.
type route struct {
	method  string
	path    string
	access  access
	handler http.HandlerFunc
}

// routes is the whole public API.
func (h *handler) routes() []route {
	return []route{
		{http.MethodGet, "/health", public, h.health},
		{http.MethodPost, "/auth/login", public, h.login},
		{http.MethodPost, "/auth/logout", authenticated, h.logout},
		{http.MethodGet, "/auth/session", authenticated, h.session},
		{http.MethodPut, "/auth/password", authenticated, h.changePassword},
		{http.MethodGet, "/system", authenticated, h.system},
		{http.MethodGet, "/nodes/{id}/hardware", authenticated, h.nodeHardware},
		{http.MethodGet, "/tasks", authenticated, h.tasks},
		{http.MethodGet, "/tasks/{id}", authenticated, h.task},
	}
}

// NewHandler returns the API as an http.Handler, wrapped in the request ID,
// access log, panic recovery, and security header middleware.
func NewHandler(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &handler{
		backend: opts.Backend, auth: opts.Auth, users: opts.Users, audit: opts.Audit,
		log: log, tls: opts.TLS, proxies: opts.TrustedProxies,
	}

	// Routes are grouped by path, and the method is checked by the
	// group, instead of using method patterns in the mux: that keeps 405
	// responses in the standard error envelope.
	byPath := map[string][]route{}
	var paths []string
	for _, rt := range h.routes() {
		if _, seen := byPath[rt.path]; !seen {
			paths = append(paths, rt.path)
		}
		byPath[rt.path] = append(byPath[rt.path], rt)
	}
	mux := http.NewServeMux()
	for _, path := range paths {
		mux.Handle(Prefix+path, h.dispatch(byPath[path]))
	}
	mux.HandleFunc("/", httpkit.NotFound)

	var root http.Handler = mux
	root = httpkit.WithRecovery(log, root)
	root = httpkit.WithAccessLog(log, root)
	root = h.withHSTS(root)
	root = httpkit.WithSecurityHeaders(root)
	root = httpkit.WithRequestID(root)
	return root
}

// dispatch serves the routes of one path: it picks the route by method,
// enforces what the route requires, and only then calls the handler.
func (h *handler) dispatch(routes []route) http.Handler {
	allow := ""
	for _, rt := range routes {
		if allow != "" {
			allow += ", "
		}
		allow += rt.method
		if rt.method == http.MethodGet {
			allow += ", HEAD"
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		if method == http.MethodHead {
			method = http.MethodGet
		}
		for _, rt := range routes {
			if rt.method == method {
				h.serve(rt, w, r)
				return
			}
		}
		w.Header().Set("Allow", allow)
		httpkit.WriteError(w, r, http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed,
			"method "+r.Method+" is not allowed on this endpoint")
	})
}

// serve authenticates the request as far as the route requires and calls
// its handler with the identity and the agent caller in the context.
func (h *handler) serve(rt route, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := h.sourceIP(r)
	ctx = context.WithValue(ctx, sourceIPKey{}, ip)

	id, err := h.identify(r)
	switch {
	case err == nil:
		// A request authenticated by cookie that changes something
		// must come from this site.
		if id.Session.Kind == auth.CookieSession && !safeMethod(r.Method) && !sameOrigin(r) {
			h.denyCSRF(w, r.WithContext(ctx), id)
			return
		}
		ctx = context.WithValue(ctx, identityKey{}, id)
		ctx = agentproto.WithCaller(ctx, agentproto.Caller{
			ActorID: id.User.ID, ActorName: id.User.Username,
			RequestID: httpkit.RequestID(ctx), SourceIP: ip,
		})
	case rt.access == public && isUnauthenticated(err):
		// Health is the only thing skep-api asks the agent without a
		// user; it does so as itself.
		ctx = agentproto.WithCaller(ctx, agentproto.Caller{
			ActorID: agentproto.APIActor, ActorName: agentproto.APIActor,
			RequestID: httpkit.RequestID(ctx), SourceIP: ip,
		})
	default:
		h.fail(w, r.WithContext(ctx), err)
		return
	}
	rt.handler(w, r.WithContext(ctx))
}

type (
	identityKey struct{}
	sourceIPKey struct{}
)

// identityFrom returns the authenticated identity of a request, if any.
func identityFrom(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(auth.Identity)
	return id, ok
}

// sourceIPFrom returns the client address of a request.
func sourceIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(sourceIPKey{}).(string)
	return ip
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
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

// sourceIP is the address of the client. It is the TCP peer, unless the
// peer is a configured trusted proxy: then it is the last address in
// X-Forwarded-For that is not itself a trusted proxy.
func (h *handler) sourceIP(r *http.Request) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	peer := ap.Addr().Unmap().WithZone("")
	if !h.trusted(peer) {
		return peer.String()
	}
	client := peer
	hops := forwardedFor(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		client = addr.Unmap().WithZone("")
		if !h.trusted(client) {
			break
		}
	}
	return client.String()
}

func (h *handler) trusted(addr netip.Addr) bool {
	for _, p := range h.proxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	health := h.backend.Health(r.Context())
	status := http.StatusOK
	if health.Status != models.HealthOK {
		status = http.StatusServiceUnavailable
	}
	// Without a session: the status only. No version, no uptime, no
	// detail about what is wrong.
	if _, ok := identityFrom(r.Context()); !ok {
		httpkit.WriteJSON(w, r, status, models.HealthStatus{Status: health.Status})
		return
	}
	httpkit.WriteJSON(w, r, status, health)
}

func (h *handler) system(w http.ResponseWriter, r *http.Request) {
	sys, err := h.backend.System(r.Context())
	if err != nil {
		h.fail(w, r, err)
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
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, hw)
}

// tasks lists tasks, newest first.
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
		h.fail(w, r, err)
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
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, detail)
}
