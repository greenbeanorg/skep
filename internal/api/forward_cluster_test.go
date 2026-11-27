package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/internal/forward"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeMembership is the three skep-clusters' replicated state as the
// three skep-apis see it: the member entries, the API pins they register
// and the guest owners.
type fakeMembership struct {
	mu     sync.Mutex
	nodes  []peerapi.Peer
	owners map[string]peerapi.GuestOwnerAnswer
}

// fakeCluster is one node's socket.
type fakeCluster struct {
	m    *fakeMembership
	self string
	ip   string
}

func (c *fakeCluster) Peers(ctx context.Context, after uint64, wait time.Duration) (peerapi.PeersAnswer, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	a := peerapi.PeersAnswer{ClusterID: "c0c0c0c0-0000-4000-8000-000000000000", NodeID: c.self, ListenAddress: c.ip, Current: true, LeaderKnown: true,
		MembershipIndex: after + 1, Peers: append([]peerapi.Peer(nil), c.m.nodes...)}
	if s, ok := a.Self(); ok {
		a.NodeName = s.Name
	}
	return a, nil
}

func (c *fakeCluster) APIKey(_ context.Context, req peerapi.APIKeyRequest) (peerapi.APIKeyResult, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	for i := range c.m.nodes {
		if c.m.nodes[i].NodeID == c.self {
			c.m.nodes[i].APIPin = req.Pin
		}
	}
	return peerapi.APIKeyResult{Index: 1, Outcome: peerapi.APIKeyApplied}, nil
}

func (c *fakeCluster) GuestOwner(_ context.Context, id string) (peerapi.GuestOwnerAnswer, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	return c.m.owners[id], nil
}

// freePort finds a TCP port free on loopback.
func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// D46: a forwarded power action across an in-process three-node cluster
// (three skep-api stacks with their own API node keys, real node
// listeners on loopback, the registration of each key, the signed
// envelope and the receiver's checks), with the user recorded on both
// sides.
func TestForwardAcrossThreeNodes(t *testing.T) {
	e := shared(t)
	port := freePort(t)
	names := []string{"smith", "langrishe", "star"}
	ids := []string{fwSelf, fwJoin, fwStar}
	m := &fakeMembership{owners: map[string]peerapi.GuestOwnerAnswer{
		fwGuest: {Found: true, Kind: "container", Owner: fwStar, OwnerName: "star"}}}
	for i, n := range names {
		m.nodes = append(m.nodes, peerapi.Peer{NodeID: ids[i], Name: n, Address: "127.0.0.1:8444", State: "member", RaftServer: true})
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	handlers := map[string]http.Handler{}
	agents := map[string]*localGuests{}
	nodes := map[string]*forward.Node{}
	for i, n := range names {
		ip := "127.0.0.1"
		if i > 0 {
			// The nodes share loopback; each binds its own address.
			ip = []string{"", "127.0.0.2", "127.0.0.3"}[i]
			m.nodes[i].Address = ip + ":8444"
		}
		node := forward.NewNode(forward.NodeOptions{KeyDir: t.TempDir() + "/nodekey", Cluster: &fakeCluster{m: m, self: ids[i], ip: ip},
			Users: e.stack.Store, Port: port, Logger: quiet})
		agent := &localGuests{}
		opts := Options{Backend: &fakeBackend{}, Containers: agent, VMs: agent, Auth: e.authn, Users: e.users, Audit: e.access,
			AuditLog: e.access, Logger: quiet, Forward: node}
		handlers[n] = NewHandler(opts)
		opts.Forward = nil
		node.SetHandler(NewNodeHandler(opts, node.Receiver()))
		nodes[n], agents[n] = node, agent
		wg.Add(1)
		go func() { defer wg.Done(); node.Run(ctx) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		ready := true
		m.mu.Lock()
		for _, p := range m.nodes {
			ready = ready && p.APIPin != ""
		}
		m.mu.Unlock()
		for _, n := range names {
			ready = ready && nodes[n].ListenerAddr() != "" && nodes[n].Health(ctx) == ""
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %+v %q", m.nodes, nodes["star"].Health(ctx))
		}
		time.Sleep(50 * time.Millisecond)
	}
	path := Prefix + "/containers/" + fwGuest + "/actions"
	rec := e.serve(handlers["smith"], request{method: http.MethodPost, path: path, role: auth.RoleOperator, body: `{"action":"stop"}`})
	if rec.Code != http.StatusAccepted || rec.Header().Get(models.NodeHeader) != fwStar {
		t.Fatalf("forwarded power action: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rid := rec.Header().Get("X-Request-ID")
	c := agents["star"].caller
	if agents["smith"].calls != 0 || agents["star"].calls != 1 || c.ActorID != e.ids[auth.RoleOperator] || c.OriginNode != fwSelf ||
		!strings.HasPrefix(c.Envelope, "v1.") || c.RequestID != rid {
		t.Fatalf("star's agent: %d calls, caller %+v (smith's %d)", agents["star"].calls, c, agents["smith"].calls)
	}
	// Both sides recorded the user with the same request ID: smith's
	// forward.send, star's container.stop with the origin node.
	if e.auditCount(t, "request_id = ? AND action = ? AND actor_id = ?", rid, ActionForwardSend, e.ids[auth.RoleOperator]) != 1 ||
		e.auditCount(t, "request_id = ? AND action = ? AND actor_id = ? AND details LIKE ?", rid, "container.stop", e.ids[auth.RoleOperator], "%\"origin_node\":\""+fwSelf+"\"%") != 1 {
		t.Fatal("the user is not recorded on both sides")
	}
	// The read through langrishe; the task through smith with ?node=.
	if rec := e.serve(handlers["langrishe"], request{method: http.MethodGet, path: Prefix + "/containers/" + fwGuest, role: auth.RoleViewer}); rec.Code != 200 || rec.Header().Get(models.NodeHeader) != fwStar {
		t.Fatalf("read through langrishe: %d", rec.Code)
	}
	// The guest owner on smith itself: served by smith's agent.
	m.mu.Lock()
	m.owners[fwGuest] = peerapi.GuestOwnerAnswer{Found: true, Kind: "container", Owner: fwSelf}
	m.mu.Unlock()
	if rec := e.serve(handlers["smith"], request{method: http.MethodGet, path: Prefix + "/containers/" + fwGuest, role: auth.RoleViewer}); rec.Code != 200 || rec.Header().Get(models.NodeHeader) != "" || agents["smith"].calls != 1 {
		t.Fatalf("local: %d", rec.Code)
	}
}
