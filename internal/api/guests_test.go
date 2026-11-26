package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/guestinv"
	"github.com/greenbeanorg/skep/internal/guestinv/guestinvtest"
	"github.com/greenbeanorg/skep/pkg/models"
)

// V5, D32: every route declares whether it is served without quorum, and
// the served set is exactly the reviewed one.
func TestEveryRouteDeclaresItsQuorumRule(t *testing.T) {
	h := &handler{}
	var served []string
	for _, rt := range h.routes() {
		switch rt.quorum {
		case quorumUndeclared:
			t.Errorf("%s %s does not declare whether it is served without quorum", rt.method, rt.path)
		case servedWithoutQuorum:
			if rt.method != http.MethodGet {
				served = append(served, rt.method+" "+rt.path)
			}
		case needsQuorum:
			if rt.method == http.MethodGet && rt.path != "/vms/{id}/console/serial" {
				t.Errorf("GET %s needs quorum; every GET but the console is served", rt.path)
			}
		}
	}
	sort.Strings(served)
	want := "POST /auth/login, POST /auth/logout, POST /containers/{id}/actions, POST /tasks/{id}/cancel, POST /vms/{id}/actions"
	if got := strings.Join(served, ", "); got != want {
		t.Errorf("served without quorum: %s\nwant %s", got, want)
	}
}

// fakeGuests is the agent's VM and jail side for the guest tests: an
// in-memory host. refuse makes the next create answer that error; lose
// makes it accept but lose the answer (D27 4c).
type fakeGuests struct {
	noVMs
	noContainers
	mu     sync.Mutex
	cts    map[string]models.Container
	tasks  map[string]models.Task // by idempotency key
	calls  int
	refuse error
	lose   bool
	n      int
	// rename, when set, is the name the host gives a created guest.
	rename string
}

func newFakeGuests() *fakeGuests {
	return &fakeGuests{cts: map[string]models.Container{}, tasks: map[string]models.Task{}}
}

func (f *fakeGuests) CreateContainer(ctx context.Context, req models.ContainerCreate) (models.ContainerCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.refuse != nil {
		return models.ContainerCreated{}, f.refuse
	}
	caller, _ := agentproto.CallerFrom(ctx)
	if t, ok := f.tasks[caller.IdempotencyKey]; ok { // a replay
		for _, c := range f.cts {
			if c.Name == req.Name {
				return models.ContainerCreated{Container: c, Task: t}, nil
			}
		}
	}
	for _, c := range f.cts {
		if c.Name == req.Name {
			return models.ContainerCreated{}, &models.Error{Code: models.CodeContainerNameTaken, Message: "another container already has this name"}
		}
	}
	f.n++
	c := models.Container{ID: fmt.Sprintf("d0000000-0000-4000-8000-%012d", f.n), Name: req.Name}
	if f.rename != "" {
		c.Name = f.rename
	}
	t := models.Task{ID: fmt.Sprintf("e0000000-0000-4000-8000-%012d", f.n), Status: models.TaskPending}
	f.cts[c.ID], f.tasks[caller.IdempotencyKey] = c, t
	if f.lose {
		return models.ContainerCreated{}, context.DeadlineExceeded
	}
	return models.ContainerCreated{Container: c, Task: t}, nil
}

func (f *fakeGuests) Container(_ context.Context, id string) (models.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cts[id]; ok {
		return c, nil
	}
	return models.Container{}, models.ErrNotFound
}

func (f *fakeGuests) Containers(context.Context, models.ContainerListOptions) (models.ContainerList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := models.ContainerList{Items: []models.Container{}}
	for _, c := range f.cts {
		out.Items = append(out.Items, c)
	}
	return out, nil
}

func (f *fakeGuests) Task(context.Context, string) (models.TaskDetail, error) {
	return models.TaskDetail{}, models.ErrNotFound
}

func (f *fakeGuests) TaskByKey(_ context.Context, key string) (models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[key]; ok {
		return t, nil
	}
	return models.Task{}, models.ErrNotFound
}

// guestRig is the API with a real identity stack, the fake host, and the
// inventory on a real state machine; rec is a reconciler on the same.
type guestRig struct {
	e      *env
	h      http.Handler
	host   *fakeGuests
	cl     *guestinvtest.Cluster
	sock   *guestinvtest.Socket
	inv    *guestinv.Inventory
	rec    *guestinv.Reconciler
	quorum bool
	now    time.Time
}

func newGuestRig(t *testing.T, e *env) *guestRig {
	g := &guestRig{e: e, host: newFakeGuests(), cl: guestinvtest.New(), quorum: true, now: time.Unix(1_900_000_000, 0)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	g.sock = g.cl.Node(guestinvtest.NodeA)
	g.inv = guestinv.New(g.sock, quiet)
	g.rec = guestinv.NewReconciler(guestinv.ReconcilerOptions{Agent: g.host, Inventory: g.inv, Logger: quiet,
		NodeID: func(context.Context) (string, error) { return guestinvtest.NodeA, nil },
		Quorum: func() bool { return g.quorum }, Now: func() time.Time { return g.now }})
	g.h = NewHandler(Options{Backend: newBackend(), VMs: g.host, Containers: g.host, Auth: e.authn, Users: e.users, Audit: e.access,
		AuditLog: e.access, Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		Quorum: func() bool { return g.quorum }, Guests: guestinv.Service{Inventory: g.inv, Reconciler: g.rec}})
	return g
}

func (g *guestRig) create(t *testing.T, name, key string) (int, models.ErrorBody, models.ContainerCreated) {
	t.Helper()
	rq := request{method: http.MethodPost, path: Prefix + "/containers", role: auth.RoleAdmin, body: `{"name":"` + name + `","template_id":"x"}`}
	if key != "" {
		rq.headers = map[string]string{"Idempotency-Key": key}
	}
	rec := g.e.serve(g.h, rq)
	var created models.ContainerCreated
	var eb models.ErrorBody
	if rec.Code == http.StatusAccepted {
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
	} else {
		eb = errorOf(t, rec.Body.Bytes())
	}
	return rec.Code, eb, created
}

func (g *guestRig) inventory() clusterstate.Guests { return g.cl.Machine().Guests() }

// D27, D28: a create reserves, calls the agent, and adds the guest with
// its task; a second create of the name is container_name_taken naming
// the node, and the agent is not called; a replay with the client's key
// is the same task and one entry; an agent refusal releases.
func TestCreateReservesAndAdds(t *testing.T) {
	g := newGuestRig(t, shared(t))
	code, _, created := g.create(t, "web1", "")
	inv := g.inventory()
	if code != http.StatusAccepted || len(inv.Guests) != 1 || inv.Guests[0].ID != created.Container.ID || inv.Guests[0].TaskID != created.Task.ID ||
		len(inv.Reservations) != 0 || inv.Guests[0].Owner != guestinvtest.NodeA {
		t.Fatalf("create: %d %+v", code, inv)
	}
	calls := g.host.calls
	code, eb, _ := g.create(t, "web1", "")
	if code != http.StatusConflict || eb.Code != models.CodeContainerNameTaken || !strings.Contains(eb.Message, "smith") || g.host.calls != calls {
		t.Fatalf("second create: %d %+v (agent calls %d→%d)", code, eb, calls, g.host.calls)
	}
	// A client key, repeated: the same task, one entry.
	_, _, a := g.create(t, "app1", "client-key-1")
	_, _, b := g.create(t, "app1", "client-key-1")
	if a.Task.ID == "" || a.Task.ID != b.Task.ID || len(g.inventory().Guests) != 2 {
		t.Fatalf("replay: %+v %+v %+v", a.Task, b.Task, g.inventory())
	}
	// The agent refuses: the reservation is released, the agent's error
	// answered.
	g.host.refuse = &models.Error{Code: models.CodeNodeDraining, Message: "draining", Details: map[string]any{"reason": "upgrade"}}
	code, eb, _ = g.create(t, "db1", "")
	g.host.refuse = nil
	if code != http.StatusServiceUnavailable || eb.Code != models.CodeNodeDraining || len(g.inventory().Reservations) != 0 {
		t.Fatalf("refused by the agent: %d %+v %+v", code, eb, g.inventory())
	}
	// Nothing sent (the socket could not be dialled): released too.
	g.host.refuse = fmt.Errorf("%w: %w (dial)", agentclient.ErrUnavailable, agentclient.ErrNotSent)
	code, _, _ = g.create(t, "db2", "")
	g.host.refuse = nil
	if code != http.StatusServiceUnavailable || len(g.inventory().Reservations) != 0 {
		t.Fatalf("not sent: %d %+v", code, g.inventory())
	}
}

// D28: a reserve that cannot be committed is 503 no_quorum with its
// outcome, and the agent is not called.
func TestCreateWithoutCommitDoesNotCallTheAgent(t *testing.T) {
	g := newGuestRig(t, shared(t))
	g.sock.Down = true
	code, eb, _ := g.create(t, "web1", "")
	if code != http.StatusServiceUnavailable || eb.Code != auth.CodeNoQuorum || eb.Details["outcome"] != auth.OutcomeNotWritten || g.host.calls != 0 {
		t.Fatalf("%d %+v, agent calls %d", code, eb, g.host.calls)
	}
}

// V2: D27 stopped after each of its steps, and what the next
// reconciliation round does.
func TestCreateStoppedAfterEachStep(t *testing.T) {
	ctx := context.Background()
	t.Run("after the reserve", func(t *testing.T) {
		// skep-api stopped after the reserve: the agent never heard of
		// it. A young reservation stays; after five minutes the round
		// releases it.
		g := newGuestRig(t, shared(t))
		if _, err := g.inv.Reserve(ctx, clusterstate.KindContainer, "web1", "k-1", "user:test-admin", ""); err != nil {
			t.Fatal(err)
		}
		if res := g.rec.Round(ctx); res.Release != 0 || len(g.inventory().Reservations) != 1 {
			t.Fatalf("young: %+v", res)
		}
		g.now = g.now.Add(6 * time.Minute)
		if res := g.rec.Round(ctx); res.Release != 1 || len(g.inventory().Reservations) != 0 {
			t.Fatalf("old: %+v %+v", res, g.inventory())
		}
	})
	t.Run("after the agent's acceptance", func(t *testing.T) {
		// The agent accepted, the answer was lost (4c): nothing more is
		// written; the round adds the guest with the reservation's task.
		g := newGuestRig(t, shared(t))
		g.host.lose = true
		if code, _, _ := g.create(t, "web1", ""); code == http.StatusAccepted {
			t.Fatal("a lost answer was answered 202")
		}
		inv := g.inventory()
		if len(inv.Reservations) != 1 || len(inv.Guests) != 0 {
			t.Fatalf("after the lost answer: %+v", inv)
		}
		res := g.rec.Round(ctx)
		inv = g.inventory()
		if res.Added != 1 || len(inv.Reservations) != 0 || len(inv.Guests) != 1 || inv.Guests[0].TaskID == "" || inv.Guests[0].Key == "" {
			t.Fatalf("round: %+v %+v", res, inv)
		}
	})
	t.Run("after the add", func(t *testing.T) {
		// Everything was written: the round writes nothing.
		g := newGuestRig(t, shared(t))
		if code, _, _ := g.create(t, "web1", ""); code != http.StatusAccepted {
			t.Fatal(code)
		}
		before := g.sock.Writes
		if res := g.rec.Round(ctx); res.Added+res.Removed+res.Release != 0 || g.sock.Writes != before {
			t.Fatalf("round: %+v", res)
		}
	})
}

// V5: the gate's refused classes. Without quorum a normal session reads
// (with Skep-Quorum: none), runs power actions, and is refused a create
// and the serial console with reason no_quorum; with quorum back a
// degraded session is still refused a create and a user create, reason
// degraded_session, audited under its user.
func TestQuorumGate(t *testing.T) {
	g := newGuestRig(t, shared(t))
	g.quorum = false
	rec := g.e.serve(g.h, request{method: http.MethodGet, path: Prefix + "/containers", role: auth.RoleAdmin})
	if rec.Code != http.StatusOK || rec.Header().Get(QuorumHeader) != "none" {
		t.Fatalf("GET without quorum: %d %q", rec.Code, rec.Header().Get(QuorumHeader))
	}
	rec = g.e.serve(g.h, request{method: http.MethodPost, path: Prefix + "/containers/d0000000-0000-4000-8000-000000000009/actions",
		role: auth.RoleAdmin, body: `{"action":"start"}`})
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("a power action without quorum: %d %s", rec.Code, rec.Body)
	}
	for _, rq := range []request{
		{method: http.MethodPost, path: Prefix + "/containers", role: auth.RoleAdmin, body: `{"name":"x","template_id":"t"}`},
		{method: http.MethodGet, path: Prefix + "/vms/d0000000-0000-4000-8000-000000000009/console/serial", role: auth.RoleAdmin},
	} {
		rec := g.e.serve(g.h, rq)
		eb := errorOf(t, rec.Body.Bytes())
		if rec.Code != http.StatusServiceUnavailable || eb.Code != auth.CodeNoQuorum || eb.Details["reason"] != "no_quorum" ||
			eb.Details["outcome"] != auth.OutcomeNotWritten || rec.Header().Get(QuorumHeader) != "none" {
			t.Errorf("%s %s: %d %s", rq.method, rq.path, rec.Code, rec.Body)
		}
	}
	if g.host.calls != 0 || g.sock.Writes != 0 {
		t.Fatal("a refused request reached the agent or the inventory")
	}
	if en := lastAudit(t, g.e, ActionNoQuorum); en.ActorName != "test-admin" || en.Details["reason"] != "no_quorum" {
		t.Errorf("audit: %+v", en)
	}

	e := freshEnv(t)
	g = newGuestRig(t, e)
	if _, err := e.db.Exec(`UPDATE sessions SET degraded = 1 WHERE token_hash = ?`, auth.HashToken(e.tokens[auth.RoleAdmin])); err != nil {
		t.Fatal(err)
	}
	for _, rq := range []request{
		{method: http.MethodPost, path: Prefix + "/containers", role: auth.RoleAdmin, body: `{"name":"x","template_id":"t"}`},
		{method: http.MethodPost, path: Prefix + "/users", role: auth.RoleAdmin, body: `{"username":"zed","password":"` + testPassword + `","role":"viewer"}`},
	} {
		rec := e.serve(g.h, rq)
		eb := errorOf(t, rec.Body.Bytes())
		if rec.Code != http.StatusServiceUnavailable || eb.Details["reason"] != "degraded_session" || rec.Header().Get(QuorumHeader) != "" {
			t.Errorf("%s %s degraded: %d %s %q", rq.method, rq.path, rec.Code, rec.Body, rec.Header().Get(QuorumHeader))
		}
	}
	if rec := e.serve(g.h, request{method: http.MethodGet, path: Prefix + "/containers", role: auth.RoleAdmin}); rec.Code != http.StatusOK {
		t.Errorf("GET for a degraded session: %d", rec.Code)
	}
	// Another session of the same server, not degraded, is served.
	if code, _, _ := g.create(t, "x", ""); code != http.StatusServiceUnavailable {
		t.Errorf("the degraded admin was served: %d", code)
	}
	rec = e.serve(g.h, request{method: http.MethodPost, path: Prefix + "/containers", role: auth.RoleOperator, body: `{"name":"y","template_id":"t"}`})
	if rec.Code != http.StatusAccepted {
		t.Errorf("a normal session with quorum: %d %s", rec.Code, rec.Body)
	}
}

// D34: the list needs system:read, vm:read and container:read; it shows
// guests (present, flag) and reservations (creating), and answers 503
// no_quorum when the socket cannot be reached.
func TestClusterGuests(t *testing.T) {
	g := newGuestRig(t, shared(t))
	ctx := context.Background()
	if _, err := g.inv.Reserve(ctx, clusterstate.KindVM, "db1", "k-1", "user:test-admin", ""); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := g.create(t, "web1", ""); code != http.StatusAccepted {
		t.Fatal(code)
	}
	other := guestinv.New(g.cl.Node(guestinvtest.NodeB), nil)
	if _, err := other.Add(ctx, "d0000000-0000-4000-8000-0000000000ff", clusterstate.KindContainer, "web1", "", "reconciliation", ""); err != nil {
		t.Fatal(err)
	}
	rec := g.e.serve(g.h, request{method: http.MethodGet, path: Prefix + "/cluster/guests", role: auth.RoleViewer})
	var list models.ClusterGuestList
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 3 || list.ClusterID != guestinvtest.ClusterID {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	states := map[string]int{}
	for _, it := range list.Items {
		states[it.State]++
		if it.Name == "web1" && it.Flagged != (it.Owner.NodeName == "langrishe") {
			t.Errorf("flag: %+v", it)
		}
	}
	if states[models.GuestPresent] != 2 || states[models.GuestCreating] != 1 {
		t.Errorf("states: %v", states)
	}
	g.sock.Down = true
	if rec := g.e.serve(g.h, request{method: http.MethodGet, path: Prefix + "/cluster/guests", role: auth.RoleViewer}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("socket down: %d", rec.Code)
	}
}

// F3 of the 8d-1 review: guest.add carries the name the agent reports;
// when it differs, the requested name's reservation is released.
func TestCreateAddsTheAgentsName(t *testing.T) {
	g := newGuestRig(t, shared(t))
	g.host.rename = "web1-agent"
	code, _, created := g.create(t, "web1", "")
	inv := g.inventory()
	if code != http.StatusAccepted || len(inv.Guests) != 1 || inv.Guests[0].ID != created.Container.ID ||
		inv.Guests[0].Name != "web1-agent" || len(inv.Reservations) != 0 {
		t.Fatalf("create: %d %+v", code, inv)
	}
}

// F7 of the 8d-1 review: a name that fails the agents' rule is 400
// invalid_request before anything is written or sent, audited as the
// create path audits an invalid request.
func TestCreateChecksTheNameFirst(t *testing.T) {
	e := freshEnv(t)
	g := newGuestRig(t, e)
	for _, name := range []string{"", "a b", "-x", strings.Repeat("a", 64)} {
		code, eb, _ := g.create(t, name, "")
		if code != http.StatusBadRequest || eb.Code != models.CodeInvalidRequest {
			t.Errorf("%q: %d %+v", name, code, eb)
		}
	}
	if g.host.calls != 0 || g.sock.Calls != 0 {
		t.Fatalf("an invalid name reached the agent (%d) or skep-cluster (%d)", g.host.calls, g.sock.Calls)
	}
	if en := lastAudit(t, e, models.TaskContainerCreate); en.Outcome != "failure" || en.Details["error_code"] != models.CodeInvalidRequest {
		t.Errorf("audit: %+v", en)
	}
	if code, _, _ := g.create(t, strings.Repeat("a", 63), ""); code != http.StatusAccepted {
		t.Errorf("a valid 63-character name: %d", code)
	}
}
