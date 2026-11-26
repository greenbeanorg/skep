// Package api is the HTTP layer of the Skep REST API under /api/v1.
//
// Handlers translate between HTTP and the service layer and contain no
// business logic. skep-api serves this API. Its Backend is the client for
// the agent's socket (internal/agentclient), so nothing here touches the
// host; identity comes from internal/auth.
//
// Every route is declared in one table together with the permission it
// requires. Enforcement is in one place (serve) and denies by default: a
// route without a known requirement is refused for everyone, and a test
// fails if the table contains one. The agent makes no authorization
// decision; this is where they are made.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/audit"
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
	// CancelTask asks for a task to be cancelled for the caller in ctx.
	CancelTask(ctx context.Context, id string) (models.Task, error)
}

// nodeIDRE is the accepted shape of a resource ID in a URL.
var nodeIDRE = regexp.MustCompile(httpkit.IDPattern)

// Options configures the public API handler.
type Options struct {
	// Backend is the service layer, in skep-api the agent client.
	Backend Backend
	// ConsoleSessionCheck is how often an open console re-checks its
	// session. Zero means DefaultConsoleSessionCheck; tests shorten it.
	ConsoleSessionCheck time.Duration
	// VMs is the VM and storage service layer. When nil, Backend is used
	// if it implements VMBackend, as the agent client does.
	VMs VMBackend
	// Templates is the jail template service layer. When nil, Backend is
	// used if it implements TemplateBackend, as the agent client does.
	Templates TemplateBackend
	// Containers is the jail service layer. When nil, Backend is used if
	// it implements ContainerBackend, as the agent client does.
	Containers ContainerBackend
	// Auth checks sessions and performs logins.
	Auth *auth.Authenticator
	// Users manages accounts.
	Users *auth.Users
	// Audit is the access audit log.
	Audit auth.AuditWriter
	// AuditLog reads the access audit log.
	AuditLog AuditReader
	// Checks are skep-api's own health checks, by name, for example the
	// access audit archive's. They are merged into the health document,
	// whose detail only an authenticated caller sees, and into the system
	// document. They are not critical: a failure degrades the document
	// but never makes the health endpoint answer 503. Optional.
	Checks map[string]models.HealthProbe
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
	// Cluster is skep-cluster's socket for GET /cluster and the member
	// operations (step 8c-2). Nil: every one of them answers 503
	// no_quorum.
	Cluster ClusterBackend
	// UI serves every path outside /api: the web interface
	// (internal/webui). It sets its own security headers. Nil, the
	// default, answers those paths with the JSON 404. An unknown path
	// under /api is a JSON 404 in either case; it never falls back to UI.
	UI http.Handler
}

type handler struct {
	backend Backend
	vms     VMBackend
	// templates is the jail template backend.
	templates TemplateBackend
	// containers is the jail backend.
	containers ContainerBackend
	// consoleCheck is how often an open console re-checks its session.
	consoleCheck time.Duration
	auth         *auth.Authenticator
	users        *auth.Users
	audit        auth.AuditWriter
	history      AuditReader
	checks       map[string]models.HealthProbe
	log          *slog.Logger
	tls          func() models.TLSInfo
	proxies      []netip.Prefix
	cluster      ClusterBackend
}

// AuditReader reads pages of the access audit log.
type AuditReader interface {
	List(ctx context.Context, limit int, before int64) (entries []models.AuditEntry, next int64, err error)
}

// What a route can require instead of a permission.
const (
	// requirePublic routes need no session. There are two: the health
	// status and the login.
	requirePublic = "public"
	// requireSession routes need a valid session and nothing more: they
	// concern only the caller's own session and password.
	requireSession = "session"
)

// route is one entry of the route table.
type route struct {
	method string
	path   string
	// requires is requirePublic, requireSession, or a permission from
	// internal/auth. Anything else, including the empty string, denies.
	requires string
	// also lists further permissions the route requires, all of them:
	// a clone needs vm:snapshot as well as vm:write (ADR 0010 decision
	// 14).
	also    []string
	handler http.HandlerFunc
}

// permissions is everything a route requires.
func (rt route) permissions() []string { return append([]string{rt.requires}, rt.also...) }

// routes is the whole public API.
func (h *handler) routes() []route {
	return append([]route{
		{http.MethodGet, "/health", requirePublic, nil, h.health},
		{http.MethodPost, "/auth/login", requirePublic, nil, h.login},
		{http.MethodPost, "/auth/logout", requireSession, nil, h.logout},
		{http.MethodGet, "/auth/session", requireSession, nil, h.session},
		{http.MethodPut, "/auth/password", requireSession, nil, h.changePassword},

		{http.MethodGet, "/system", auth.PermSystemRead, nil, h.system},
		{http.MethodGet, "/nodes/{id}/hardware", auth.PermNodeRead, nil, h.nodeHardware},
		{http.MethodGet, "/tasks", auth.PermTaskRead, nil, h.tasks},
		{http.MethodGet, "/tasks/{id}", auth.PermTaskRead, nil, h.task},
		{http.MethodPost, "/tasks/{id}/cancel", auth.PermTaskCancel, nil, h.cancelTask},

		{http.MethodGet, "/users", auth.PermUserRead, nil, h.listUsers},
		{http.MethodPost, "/users", auth.PermUserManage, nil, h.createUser},
		{http.MethodGet, "/users/{id}", auth.PermUserRead, nil, h.getUser},
		{http.MethodPut, "/users/{id}", auth.PermUserManage, nil, h.updateUser},
		{http.MethodDelete, "/users/{id}", auth.PermUserManage, nil, h.deleteUser},
		{http.MethodGet, "/roles", auth.PermRoleRead, nil, h.listRoles},
		{http.MethodGet, "/audit", auth.PermAuditRead, nil, h.listAudit},
	}, append(append(append(h.vmRoutes(), h.templateRoutes()...), h.containerRoutes()...), h.clusterRoutes()...)...)
}

// NewHandler returns the API as an http.Handler, wrapped in the request ID,
// access log, panic recovery, and security header middleware.
func NewHandler(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	h := &handler{
		backend: opts.Backend, auth: opts.Auth, users: opts.Users, audit: opts.Audit, history: opts.AuditLog,
		checks: opts.Checks, log: log, tls: opts.TLS, proxies: opts.TrustedProxies,
	}
	h.cluster = opts.Cluster
	if h.cluster == nil {
		h.cluster = noCluster{}
	}
	h.consoleCheck = opts.ConsoleSessionCheck
	if h.consoleCheck <= 0 {
		h.consoleCheck = DefaultConsoleSessionCheck
	}
	switch vms, ok := opts.Backend.(VMBackend); {
	case opts.VMs != nil:
		h.vms = opts.VMs
	case ok:
		h.vms = vms
	default:
		h.vms = noVMs{}
	}
	switch tpl, ok := opts.Backend.(TemplateBackend); {
	case opts.Templates != nil:
		h.templates = opts.Templates
	case ok:
		h.templates = tpl
	default:
		h.templates = noTemplates{}
	}
	switch ct, ok := opts.Backend.(ContainerBackend); {
	case opts.Containers != nil:
		h.containers = opts.Containers
	case ok:
		h.containers = ct
	default:
		h.containers = noContainers{}
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
	// Anything else under /api, any version, is an unknown endpoint. The
	// interface, when there is one, gets the rest. "/api" itself is
	// registered too, or the mux would redirect it to "/api/".
	mux.HandleFunc("/api", httpkit.NotFound)
	mux.HandleFunc("/api/", httpkit.NotFound)
	// The usual health-check paths are not the interface: its 200 would
	// read as a passing check whatever the state of the service.
	// With a trailing slash too ("{$}": that path only, not the subtree).
	mux.HandleFunc("/health", healthMoved)
	mux.HandleFunc("/healthz", healthMoved)
	mux.HandleFunc("/health/{$}", healthMoved)
	mux.HandleFunc("/healthz/{$}", healthMoved)
	if opts.UI != nil {
		mux.Handle("/", opts.UI)
	} else {
		mux.HandleFunc("/", httpkit.NotFound)
	}

	var root http.Handler = mux
	root = httpkit.WithRecovery(log, root)
	root = httpkit.WithAccessLog(log, root)
	root = h.withHSTS(root)
	root = httpkit.WithSecurityHeaders(root)
	root = httpkit.WithRequestID(root)
	return root
}

// healthMoved answers the usual health-check paths with a 404 that names
// the real endpoint.
func healthMoved(w http.ResponseWriter, r *http.Request) {
	httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound,
		"no such endpoint: "+r.URL.Path+"; the health endpoint is "+Prefix+"/health")
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

// serve authenticates the request, checks what the route requires, and
// only then calls its handler, with the identity and the agent caller in
// the context. It is the one place where access is decided.
func (h *handler) serve(rt route, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := h.sourceIP(r)
	ctx = context.WithValue(ctx, sourceIPKey{}, ip)

	id, err := h.identify(r)
	// Any request that names an origin must name this one, whatever the
	// route, the method, or the credentials: also the login, the logout,
	// and bearer requests. Requests without Origin (skepctl, curl) are
	// not affected; for cookie writes, the check below requires one.
	if r.Header.Get("Origin") != "" && !sameOrigin(r) {
		var who *auth.Identity
		if err == nil {
			who = &id
		}
		h.denyOrigin(w, r.WithContext(ctx), who)
		return
	}
	switch {
	case err == nil:
		// A request authenticated by cookie that changes something
		// must come from this site.
		if id.Session.Kind == auth.CookieSession && !safeMethod(r.Method) && !sameOrigin(r) {
			h.denyOrigin(w, r.WithContext(ctx), &id)
			return
		}
		ctx = context.WithValue(ctx, identityKey{}, id)
		ctx = agentproto.WithCaller(ctx, agentproto.Caller{
			ActorID: id.User.ID, ActorName: id.User.Username,
			RequestID: httpkit.RequestID(ctx), SourceIP: ip,
		})
	case rt.requires == requirePublic && isUnauthenticated(err):
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
	r = r.WithContext(ctx)

	switch rt.requires {
	case requirePublic:
	case requireSession:
		// Reaching this point without an identity is impossible: the
		// switch above returned. Checked anyway, because this is the
		// gate.
		if _, ok := identityFrom(ctx); !ok {
			h.fail(w, r, auth.ErrUnauthenticated)
			return
		}
	default:
		// A permission. The role was read from the database for this
		// request, so a role change is effective at once. Deny by
		// default: an unknown requirement allows nobody.
		if _, ok := identityFrom(ctx); !ok {
			h.fail(w, r, auth.ErrUnauthenticated)
			return
		}
		for _, p := range rt.permissions() {
			if !id.User.Role.Allows(p) {
				h.deny(w, r, id, rt, p)
				return
			}
		}
	}
	rt.handler(w, r)
}

// requireAlso checks a permission that depends on the body of a request,
// such as vm:delete for an update that removes disks (ADR 0010 decision
// 4), after the route's own. A refusal is the same 403, audited the same
// way, as the route check's.
func (h *handler) requireAlso(w http.ResponseWriter, r *http.Request, method, path, permission string) bool {
	id, ok := identityFrom(r.Context())
	if !ok {
		h.fail(w, r, auth.ErrUnauthenticated)
		return false
	}
	if id.User.Role.Allows(permission) {
		return true
	}
	h.deny(w, r, id, route{method: method, path: path, requires: permission}, permission)
	return false
}

// deny refuses a request for lack of a permission and audits the denial
// with the actor, the permission, and the route.
func (h *handler) deny(w http.ResponseWriter, r *http.Request, id auth.Identity, rt route, permission string) {
	h.writeAudit(r, audit.AccessEntry{
		ActorID: id.User.ID, ActorName: id.User.Username, Action: auth.ActionDenied, Outcome: audit.Denied,
		Details: map[string]any{
			"permission": permission, "method": rt.method, "route": Prefix + rt.path, "role": id.User.Role.Name,
		},
	})
	h.log.WarnContext(r.Context(), "request denied", "user", id.User.Username, "role", id.User.Role.Name,
		"permission", permission, "method", rt.method, "route", Prefix+rt.path)
	httpkit.WriteErrorDetails(w, r, http.StatusForbidden, auth.CodeForbidden,
		"your role does not allow this", map[string]any{"permission": permission})
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

// healthDocument is the agent's health document with skep-api's own
// checks merged in.
func (h *handler) healthDocument(ctx context.Context) models.Health {
	health := h.backend.Health(ctx)
	// Normalise what the agent said, so an empty status reads as ok.
	health.Status = models.WorseHealth(health.Status, models.HealthOK)
	// skep-api's own checks, next to the agent's. None of them is
	// critical: skep-api serves requests while they fail.
	for name, probe := range h.checks {
		health.Record(name, false, probe(ctx))
	}
	return health
}

// health answers 503 only when the service cannot serve requests: when a
// critical check failed. A degraded component, such as a late audit
// archive, is reported in the body, which monitoring checks deliberately,
// and in the system document; the endpoint stays at 200 so that a load
// balancer or a supervisor does not take a working service away.
func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	health := h.healthDocument(r.Context())
	status := http.StatusOK
	if health.Status == models.HealthUnavailable {
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
	health := h.healthDocument(r.Context())
	sys.Health = &health
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

// ActionTaskCancel is the access audit action of a cancel request. The
// cancellation itself is recorded by the agent, in its own audit log, with
// the same request ID.
const ActionTaskCancel = "task.cancel"

// cancelTask asks the agent to cancel a task on behalf of the caller. It
// is the first endpoint that changes something on the node. The agent
// records the caller as the actor of the cancellation.
func (h *handler) cancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid task ID")
		return
	}
	who, _ := identityFrom(r.Context())
	entry := audit.AccessEntry{
		ActorID: who.User.ID, ActorName: who.User.Username, Action: ActionTaskCancel,
		ResourceType: "task", ResourceID: id, Outcome: audit.Success,
	}
	task, err := h.backend.CancelTask(r.Context(), id)
	if err != nil {
		entry.Outcome = audit.Failure
		var me *models.Error
		if errors.As(err, &me) {
			entry.Details = map[string]any{"error_code": me.Code}
		} else if errors.Is(err, models.ErrNotFound) {
			entry.Details = map[string]any{"error_code": httpkit.CodeNotFound}
		}
		h.writeAudit(r, entry)
		h.fail(w, r, err)
		return
	}
	entry.Details = map[string]any{"task_type": task.Type, "task_status": string(task.Status)}
	h.writeAudit(r, entry)
	httpkit.WriteJSON(w, r, http.StatusOK, task)
}
