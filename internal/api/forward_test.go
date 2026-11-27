package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/internal/forward"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	fwSelf  = "a1a1a1a1-0000-4000-8000-00000000000a" // smith, this node
	fwStar  = "b2b2b2b2-0000-4000-8000-00000000000b"
	fwJoin  = "c3c3c3c3-0000-4000-8000-00000000000c"
	fwGuest = "dddddddd-0000-4000-8000-00000000000d"
	fwTask  = "eeeeeeee-0000-4000-8000-00000000000e"
)

// fakeForwarding is the origin's forwarding: a fixed peer list, guest
// owners, and a recorded Forward.
type fakeForwarding struct {
	mu     sync.Mutex
	ans    peerapi.PeersAnswer
	fresh  bool
	owners map[string]peerapi.GuestOwnerAnswer
	sent   []forward.Request
	to     []peerapi.Peer
	res    forward.Result
}

func newFakeForwarding() *fakeForwarding {
	return &fakeForwarding{fresh: true, owners: map[string]peerapi.GuestOwnerAnswer{}, res: forward.Result{Status: 202,
		Header: http.Header{"Content-Type": {"application/json"}, "X-Other": {"x"}}, Body: []byte(`{"id":"remote"}`)},
		ans: peerapi.PeersAnswer{ClusterID: "c", NodeID: fwSelf, NodeName: "smith", Current: true, Peers: []peerapi.Peer{
			{NodeID: fwSelf, Name: "smith", State: "member", APIPin: "sha256:1", RaftServer: true},
			{NodeID: fwStar, Name: "star", State: "member", APIPin: "sha256:2", RaftServer: true},
			{NodeID: fwJoin, Name: "blazanov", State: "joining", RaftServer: false},
		}}}
}

func (f *fakeForwarding) PeersNow() (peerapi.PeersAnswer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ans, f.fresh
}

func (f *fakeForwarding) GuestOwner(_ context.Context, id string) (peerapi.GuestOwnerAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.owners[id]; ok {
		return a, nil
	}
	return peerapi.GuestOwnerAnswer{}, nil
}

func (f *fakeForwarding) Forward(_ context.Context, p peerapi.Peer, req forward.Request) forward.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent, f.to = append(f.sent, req), append(f.to, p)
	return f.res
}

func (f *fakeForwarding) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// localGuests is this node's agent: it answers every guest as its own and
// records the caller.
type localGuests struct {
	noContainers
	noVMs
	mu     sync.Mutex
	caller agentproto.Caller
	calls  int
}

func (l *localGuests) record(ctx context.Context) {
	l.mu.Lock()
	l.caller, _ = agentproto.CallerFrom(ctx)
	l.calls++
	l.mu.Unlock()
}

func (l *localGuests) Container(ctx context.Context, id string) (models.Container, error) {
	l.record(ctx)
	return models.Container{ID: id, Name: "local"}, nil
}

func (l *localGuests) ContainerAction(ctx context.Context, id string, _ models.ContainerAction) (models.Task, error) {
	l.record(ctx)
	return models.Task{ID: fwTask, NodeID: fwSelf, Type: "container.stop"}, nil
}

func (l *localGuests) VM(ctx context.Context, id string) (models.VM, error) {
	l.record(ctx)
	return models.VM{ID: id, Name: "local"}, nil
}

type fwRig struct {
	e      *env
	f      *fakeForwarding
	agent  *localGuests
	tasks  *fakeBackend
	h      http.Handler
	quorum bool
}

func newFwRig(t *testing.T, e *env) *fwRig {
	r := &fwRig{e: e, f: newFakeForwarding(), agent: &localGuests{}, quorum: true,
		tasks: &fakeBackend{task: models.TaskDetail{Task: models.Task{ID: fwTask, NodeID: fwSelf, Status: models.TaskSucceeded}}}}
	r.h = NewHandler(Options{Backend: r.tasks, Containers: r.agent, VMs: r.agent, Auth: e.authn, Users: e.users, Audit: e.access,
		AuditLog: e.access, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Forward: r.f, Quorum: func() bool { return r.quorum }})
	return r
}

// D29: every route declares a scope, and exactly the six routes of this
// step are forwardable.
func TestRouteScopes(t *testing.T) {
	h := newHandler(Options{})
	want := map[string]bool{"GET /vms/{id}": true, "POST /vms/{id}/actions": true, "GET /containers/{id}": true,
		"POST /containers/{id}/actions": true, "GET /tasks/{id}": true, "POST /tasks/{id}/cancel": true}
	got := 0
	for _, rt := range h.routes() {
		key := rt.method + " " + rt.path
		if rt.scope == scopeUndeclared {
			t.Errorf("%s declares no scope", key)
		}
		if rt.fwd != want[key] {
			t.Errorf("%s: forwardable %v", key, rt.fwd)
		}
		if rt.fwd {
			got++
		}
		local := strings.HasPrefix(rt.path, "/auth/") || strings.HasPrefix(rt.path, "/users") || strings.HasPrefix(rt.path, "/cluster") ||
			rt.path == "/health" || rt.path == "/roles" || rt.path == "/audit"
		if local != (rt.scope == scopeLocal) {
			t.Errorf("%s: scope %d", key, rt.scope)
		}
	}
	if got != 6 {
		t.Fatalf("%d forwardable routes", got)
	}
	// A route that declares none is refused for everyone.
	rec := httptestRecorder()
	h.fwd = newFakeForwarding()
	if !h.routeElsewhere(route{method: "GET", path: "/x"}, rec, newRequest("GET", "/api/v1/x")) || rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared: %d", rec.Code)
	}
}

// D30: the node parameter.
func TestNodeParameter(t *testing.T) {
	r := newFwRig(t, shared(t))
	get := func(path string) (int, string) {
		rec := r.e.serve(r.h, request{method: http.MethodGet, path: Prefix + path, role: auth.RoleViewer})
		return rec.Code, rec.Body.String()
	}
	if c, b := get("/containers/" + fwGuest + "?node=star"); c != 400 || !strings.Contains(b, "this route takes no node parameter") {
		t.Fatalf("a node parameter on a guest route: %d %s", c, b)
	}
	if c, _ := get("/vms?node=" + fwSelf); c != 400 {
		t.Fatalf("on a list: %d", c)
	}
	for _, self := range []string{fwSelf, "smith"} {
		if c, _ := get("/tasks/" + fwTask + "?node=" + self); c != 200 || r.f.count() != 0 {
			t.Fatalf("this node (%s): %d, %d forwards", self, c, r.f.count())
		}
	}
	if c, _ := get("/tasks/" + fwTask + "?node=nobody"); c != 404 {
		t.Fatalf("no such node: %d", c)
	}
	if c, b := get("/tasks/" + fwTask + "?node=blazanov"); c != 503 || !strings.Contains(b, models.CodeNodeUnreachable) || !strings.Contains(b, "joining") {
		t.Fatalf("a joining node: %d %s", c, b)
	}
	rec := r.e.serve(r.h, request{method: http.MethodGet, path: Prefix + "/tasks/" + fwTask + "?node=star", role: auth.RoleViewer})
	if rec.Code != 202 || rec.Header().Get(models.NodeHeader) != fwStar || rec.Header().Get("X-Other") != "" || r.f.count() != 1 {
		t.Fatalf("star: %d %v", rec.Code, rec.Header())
	}
	sent := r.f.sent[0]
	if sent.Method != "GET" || sent.URI != Prefix+"/tasks/"+fwTask+"?node=star" || sent.UserID != r.e.ids[auth.RoleViewer] || sent.Epoch == 0 {
		t.Fatalf("sent %+v", sent)
	}
	// A cancellation by node; and a stale peer list.
	rec = r.e.serve(r.h, request{method: http.MethodPost, path: Prefix + "/tasks/" + fwTask + "/cancel?node=" + fwStar, role: auth.RoleOperator})
	if rec.Code != 202 || r.f.count() != 2 {
		t.Fatalf("cancel on star: %d", rec.Code)
	}
	// F2: the origin never builds an envelope with a malformed key.
	rec = r.e.serve(r.h, request{method: http.MethodPost, path: Prefix + "/tasks/" + fwTask + "/cancel?node=star", role: auth.RoleOperator,
		headers: map[string]string{agentproto.HeaderIdempotencyKey: "k 1"}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), httpkit.CodeInvalidRequest) || r.f.count() != 2 {
		t.Fatalf("a malformed idempotency key: %d %s", rec.Code, rec.Body)
	}
	r.f.fresh = false
	if c, b := get("/tasks/" + fwTask + "?node=star"); c != 503 || !strings.Contains(b, "no current peer list") {
		t.Fatalf("stale list: %d %s", c, b)
	}
	if c, _ := get("/tasks/" + fwTask + "?node=smith"); c != 200 {
		t.Fatalf("this node with a stale list: %d", c)
	}
}

// 8e-1 fix F1: a node parameter that names this node by its ID is served
// here from the agent's node ID, whatever the peer list says; F4: a node
// parameter of neither shape is 400 and never echoed.
func TestNodeParameterSelfAndShape(t *testing.T) {
	r := newFwRig(t, shared(t))
	r.f.ans, r.f.fresh = peerapi.PeersAnswer{}, false // no peers answer since start
	r.tasks.systemErr = errors.New("the agent is down")
	get := func(node string) (int, string) {
		rec := r.e.serve(r.h, request{method: http.MethodGet, path: Prefix + "/tasks/" + fwTask + "?node=" + node, role: auth.RoleViewer})
		return rec.Code, rec.Body.String()
	}
	if c, b := get(fwSelf); c != 503 || !strings.Contains(b, models.CodeNodeUnreachable) {
		t.Fatalf("the agent down, no peers answer: %d %s", c, b)
	}
	r.tasks.systemErr, r.tasks.system = nil, models.System{NodeID: fwSelf}
	if c, b := get(fwSelf); c != 200 || r.f.count() != 0 {
		t.Fatalf("this node's ID, no peers answer: %d %s", c, b)
	}
	if c, b := get(fwStar); c != 503 || !strings.Contains(b, models.CodeNodeUnreachable) {
		t.Fatalf("another node's ID, no peers answer: %d %s", c, b)
	}
	if c, _ := get("smith"); c != 503 {
		t.Fatalf("this node's name needs a peers answer: %d", c)
	}
	r.tasks.system = models.System{} // kept once known
	if c, _ := get(fwSelf); c != 200 {
		t.Fatalf("the kept node ID: %d", c)
	}
	for _, bad := range []string{"", "bad_name", "-x", "a%20b", "%3Cb%3E"} {
		for _, path := range []string{"/tasks/" + fwTask, "/containers/" + fwGuest} {
			rec := r.e.serve(r.h, request{method: http.MethodGet, path: Prefix + path + "?node=" + bad, role: auth.RoleViewer})
			b := rec.Body.String()
			if rec.Code != 400 || !strings.Contains(b, httpkit.CodeInvalidRequest) || !strings.Contains(b, "neither a node ID nor a node name") ||
				(bad != "" && strings.Contains(b, bad)) || strings.Contains(b, "<b>") || strings.Contains(b, "a b") {
				t.Errorf("node=%q on %s: %d %s", bad, path, rec.Code, b)
			}
		}
	}
	if r.f.count() != 0 {
		t.Fatal("forwarded")
	}
}

// D31: a guest route by its owner.
func TestGuestRoutedByOwner(t *testing.T) {
	e := shared(t)
	r := newFwRig(t, e)
	path := Prefix + "/containers/" + fwGuest
	get := func() *httptestResponse {
		return r.e.serve(r.h, request{method: http.MethodGet, path: path, role: auth.RoleViewer})
	}
	// Not found, another kind, this node, skep-cluster not answering:
	// local.
	for name, o := range map[string]peerapi.GuestOwnerAnswer{
		"not found":    {},
		"another kind": {Found: true, Kind: "vm", Owner: fwStar},
		"this node":    {Found: true, Kind: "container", Owner: fwSelf},
	} {
		r.f.owners[fwGuest] = o
		if rec := get(); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"local"`) || r.f.count() != 0 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	r.f.owners[fwGuest] = peerapi.GuestOwnerAnswer{Found: true, Kind: "container", Owner: fwStar, OwnerName: "star"}
	rec := get()
	if rec.Code != 202 || rec.Header().Get(models.NodeHeader) != fwStar || rec.Body.String() != `{"id":"remote"}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("star's guest: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	// A power action: forwarded, and audited on the origin (D36); a GET
	// is not.
	before := e.auditCount(t, "action = ?", ActionForwardSend)
	rec = r.e.serve(r.h, request{method: http.MethodPost, path: path + "/actions", role: auth.RoleOperator, body: `{"action":"stop"}`})
	if rec.Code != 202 || r.f.count() != 2 || string(r.f.sent[1].Body) != `{"action":"stop"}` || r.f.sent[1].ContentType != "application/json" {
		t.Fatalf("power action: %d %+v", rec.Code, r.f.sent)
	}
	if e.auditCount(t, "action = ? AND details LIKE ? AND details LIKE ?", ActionForwardSend, "%"+fwStar+"%", "%\"status\":202%") != before+1 {
		t.Fatal("forward.send not audited")
	}
	// A viewer's power action is refused here, never forwarded.
	if rec := r.e.serve(r.h, request{method: http.MethodPost, path: path + "/actions", role: auth.RoleViewer, body: `{"action":"stop"}`}); rec.Code != 403 || r.f.count() != 2 {
		t.Fatalf("viewer: %d", rec.Code)
	}
	// The owner not usable: 503 naming it and why, nothing sent.
	r.f.ans.Peers[1].APIPin = ""
	if rec := get(); rec.Code != 503 || !strings.Contains(rec.Body.String(), "has not registered its API key") || !strings.Contains(rec.Body.String(), `"node_name":"star"`) || r.f.count() != 2 {
		t.Fatalf("no API key: %d %s", rec.Code, rec.Body)
	}
	r.f.ans.Peers[1].APIPin = "sha256:2"
	r.f.ans.Peers[1].Clock = peerapi.Clock{Known: true, OffsetMillis: 20000}
	if rec := get(); rec.Code != 503 || !strings.Contains(rec.Body.String(), models.CodeNodeClockSkew) {
		t.Fatalf("clock: %d %s", rec.Code, rec.Body)
	}
	r.f.ans.Peers[1].Clock = peerapi.Clock{}
	r.f.fresh = false
	if rec := get(); rec.Code != 503 || !strings.Contains(rec.Body.String(), "no current peer list") {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body)
	}
	r.f.fresh = true
	// Without quorum the origin's gate is unchanged: a forwardable route
	// is served without quorum, so it is forwarded (D32).
	r.quorum = false
	if rec := get(); rec.Code != 202 {
		t.Fatalf("without quorum: %d", rec.Code)
	}
	// The receiver's error answers: stale_origin is 401.
	r.f.res = forward.Result{Err: &models.Error{Code: auth.CodeUnauthenticated}}
	if rec := get(); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("stale origin: %d", rec.Code)
	}
}

// D46: the user listener answers a request that carries a forged envelope
// exactly as without it: it never reads the header.
func TestUserListenerIgnoresAnEnvelope(t *testing.T) {
	r := newFwRig(t, shared(t))
	path := Prefix + "/containers/" + fwGuest
	for _, role := range []string{"", auth.RoleViewer} {
		plain := r.e.serve(r.h, request{method: http.MethodGet, path: path, role: role})
		forged := r.e.serve(r.h, request{method: http.MethodGet, path: path, role: role, headers: map[string]string{forward.Header: "v1.e30.AAAA"}})
		strip := func(b string) string { return regexp.MustCompile(`"request_id":"[^"]*"`).ReplaceAllString(b, "") }
		if plain.Code != forged.Code || strip(plain.Body.String()) != strip(forged.Body.String()) {
			t.Fatalf("role %q: %d %s vs %d %s", role, plain.Code, plain.Body, forged.Code, forged.Body)
		}
	}
	if rec := r.e.serve(r.h, request{method: http.MethodGet, path: path, headers: map[string]string{forward.Header: "v1.e30.AAAA"}}); rec.Code != 401 {
		t.Fatalf("no session: %d", rec.Code)
	}
}

// fakeReceiver answers Check with a fixed outcome.
type fakeReceiver struct {
	acc *forward.Accepted
	ref *forward.Refusal
	n   int
}

func (f *fakeReceiver) Check(r *http.Request) (*forward.Accepted, *forward.Refusal) {
	f.n++
	return f.acc, f.ref
}

func nodeRig(t *testing.T, e *env, quorum bool) (http.Handler, *fakeReceiver, *localGuests, *fakeForwarding) {
	agent := &localGuests{}
	f := newFakeForwarding()
	u, err := e.stack.Store.UserByID(context.Background(), e.ids[auth.RoleOperator])
	if err != nil {
		t.Fatal(err)
	}
	recv := &fakeReceiver{acc: &forward.Accepted{User: u, Envelope: &forward.Envelope{Raw: "v1.payload.signature",
		Payload: forward.Payload{Origin: fwStar, RequestID: "req-origin", SourceIP: "198.51.100.9", Target: fwSelf}}}}
	h := NewNodeHandler(Options{Backend: &fakeBackend{}, Containers: agent, VMs: agent, Auth: e.authn, Users: e.users, Audit: e.access,
		AuditLog: e.access, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Forward: f, Quorum: func() bool { return quorum }}, recv)
	return h, recv, agent, f
}

// D19, D24–D28, V1, V3: the node listener's handler.
func TestNodeHandler(t *testing.T) {
	e := shared(t)
	h, recv, agent, f := nodeRig(t, e, true)
	serve := func(method, path, body string) *httptestResponse {
		return e.serve(h, request{method: method, path: path, body: body})
	}
	// Nothing outside the six routes, whatever the credentials.
	for _, p := range []struct{ m, p string }{{"GET", "/api/v1/health"}, {"POST", "/api/v1/auth/login"}, {"GET", "/api/v1/users"},
		{"GET", "/api/v1/cluster"}, {"GET", "/api/v1/audit"}, {"GET", "/"}, {"PUT", "/api/v1/containers/" + fwGuest}, {"GET", "/api/v1/vms"},
		{"GET", "/api/v1/containers/" + fwGuest + "/snapshots"}} {
		rec := e.serve(h, request{method: p.m, path: p.p, role: auth.RoleAdmin})
		if rec.Code != 404 || rec.Header().Get(forward.RefusalHeader) != forward.CodeNoRoute {
			t.Errorf("%s %s: %d %v", p.m, p.p, rec.Code, rec.Header())
		}
	}
	if recv.n != 0 {
		t.Fatal("the receiver ran for a path it does not serve")
	}
	// Accepted: the handler runs as the user of this node's copy, with
	// the envelope recorded for the agent.
	before := e.auditCount(t, "details LIKE ?", "%\"origin_node\":\""+fwStar+"\"%")
	rec := serve("POST", "/api/v1/containers/"+fwGuest+"/actions", `{"action":"stop"}`)
	if rec.Code != 202 {
		t.Fatalf("power action: %d %s", rec.Code, rec.Body)
	}
	c := agent.caller
	if c.ActorID != e.ids[auth.RoleOperator] || c.OriginNode != fwStar || c.Envelope != "v1.payload.signature" || c.RequestID != "req-origin" || c.SourceIP != "198.51.100.9" {
		t.Fatalf("caller %+v", c)
	}
	if e.auditCount(t, "details LIKE ?", "%\"origin_node\":\""+fwStar+"\"%") != before+1 || f.count() != 0 {
		t.Fatal("the access audit entry lacks origin_node, or the receiver forwarded")
	}
	if rec := serve("GET", "/api/v1/containers/"+fwGuest, ""); rec.Code != 200 {
		t.Fatalf("read: %d", rec.Code)
	}
	// A refusal before the signature verified: the header, not audited.
	n := e.auditCount(t, "action = ?", ActionForwardRefused)
	recv.ref = &forward.Refusal{Code: forward.CodeNotMember, Message: "no"}
	if rec := serve("GET", "/api/v1/containers/"+fwGuest, ""); rec.Code != 403 || rec.Header().Get(forward.RefusalHeader) != forward.CodeNotMember || e.auditCount(t, "action = ?", ActionForwardRefused) != n {
		t.Fatalf("not member: %d", rec.Code)
	}
	// After it: audited with the origin node and the code.
	// F3: with the envelope's user ID. V1: with the origin's request ID
	// (the X-Request-ID header the origin sends), as the handler's entries
	// (the envelope's request ID, req-origin above).
	recv.ref = &forward.Refusal{Code: forward.CodeBehind, Message: "behind", Verified: true, Origin: fwStar, UserID: e.ids[auth.RoleOperator]}
	if rec := e.serve(h, request{method: "POST", path: "/api/v1/containers/" + fwGuest + "/actions", body: `{}`,
		headers: map[string]string{"X-Request-ID": "req-refused"}}); rec.Code != 503 || rec.Header().Get(forward.RefusalHeader) != forward.CodeBehind {
		t.Fatalf("behind: %d", rec.Code)
	}
	if e.auditCount(t, "action = ? AND request_id = ? AND details LIKE ? AND details LIKE ? AND details LIKE ?", ActionForwardRefused, "req-refused",
		"%\"code\":\"behind\"%", "%"+fwStar+"%", "%\"user_id\":\""+e.ids[auth.RoleOperator]+"\"%") != n+1 {
		t.Fatal("forward.refused not audited with the origin node, the user and the request ID")
	}
	if e.auditCount(t, "request_id = ? AND details LIKE ?", "req-origin", "%\"origin_node\":\""+fwStar+"\"%") == 0 {
		t.Fatal("the handler's entry lacks the origin's request ID")
	}
	// The permission check uses this node's role: a viewer's power
	// action is a 403, audited with the origin node.
	recv.ref = nil
	v, _ := e.stack.Store.UserByID(context.Background(), e.ids[auth.RoleViewer])
	recv.acc.User = v
	if rec := serve("POST", "/api/v1/containers/"+fwGuest+"/actions", `{"action":"stop"}`); rec.Code != 403 {
		t.Fatalf("viewer: %d", rec.Code)
	}
	if e.auditCount(t, "action = ? AND details LIKE ?", auth.ActionDenied, "%\"origin_node\":\""+fwStar+"\"%") == 0 {
		t.Fatal("the denial lacks origin_node")
	}
}

// D46, A5: a receiver without quorum serves a forwarded read and power
// action, and refuses a forwarded route that needs quorum (a test-only
// route: the six of this step are all served without quorum).
func TestNodeHandlerWithoutQuorum(t *testing.T) {
	testRoutes = func(h *handler) []route {
		return []route{{http.MethodPost, "/test/needs-quorum", auth.PermContainerPower, nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(299)
		}, needsQuorum, scopeNode, true}}
	}
	defer func() { testRoutes = func(*handler) []route { return nil } }()
	e := shared(t)
	h, _, _, _ := nodeRig(t, e, false)
	for _, c := range []struct {
		m, p, body string
		code       int
	}{
		{"GET", "/api/v1/containers/" + fwGuest, "", 200},
		{"POST", "/api/v1/containers/" + fwGuest + "/actions", `{"action":"start"}`, 202},
		{"POST", "/api/v1/test/needs-quorum", `{}`, 503},
	} {
		rec := e.serve(h, request{method: c.m, path: c.p, body: c.body})
		if rec.Code != c.code || rec.Header().Get(QuorumHeader) != "none" {
			t.Fatalf("%s %s: %d %v %s", c.m, c.p, rec.Code, rec.Header(), rec.Body)
		}
		if c.code == 503 && !strings.Contains(rec.Body.String(), auth.CodeNoQuorum) {
			t.Fatalf("not no_quorum: %s", rec.Body)
		}
	}
}

type httptestResponse = httptest.ResponseRecorder

func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newRequest(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }
