package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/pkg/models"
)

const nodeA = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"

// oneNode makes a one-node cluster as "skep-cluster init" does.
func oneNode(t *testing.T, pinOverride string) (*store.Node, *nodekey.Key) {
	t.Helper()
	dir := t.TempDir()
	key, err := nodekey.Create(dir, nodeA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(store.Options{Dir: dir, NodeID: nodeA}, ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.Open(store.Options{Dir: dir, NodeID: nodeA})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	pin := key.Pin()
	if pinOverride != "" {
		pin = pinOverride
	}
	e, _ := clusterstate.NewEntry(clusterstate.TypeClusterInit, "op-init", clusterstate.ActorSystem, nodeA,
		clusterstate.ClusterInit{ClusterID: "c1", Member: clusterstate.Member{ID: nodeA, Name: "smith", Pin: pin, State: clusterstate.StateMember}})
	if _, err := n.Apply(e); err != nil {
		t.Fatal(err)
	}
	return n, key
}

func run(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// A single host: no listen address, so no network listener at all.
func TestSingleHostOpensNoListener(t *testing.T) {
	n, key := oneNode(t, "")
	s := New(Options{Config: clusterconf.Config{NodeID: nodeA, NodeName: "smith"}, Key: key, Store: n, Interval: 20 * time.Millisecond})
	run(t, s)
	eventually(t, "health ok", func() bool { return s.Health(context.Background()).Status == models.HealthOK })
	time.Sleep(100 * time.Millisecond) // several rounds of background work
	st := s.Status(context.Background())
	if st.Listening || st.ListenAddress != "" {
		t.Fatalf("a single host listens: %+v", st)
	}
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	if l != nil {
		t.Fatal("a listener was opened without a listen address")
	}
	if st.NodeID != nodeA || st.ClusterID != "c1" || st.Pin != key.Pin() || len(st.Members) != 1 ||
		st.Members[0].Address != "" || st.Raft.State != "leader" {
		t.Fatalf("status %+v", st)
	}
	if cfg, _ := s.RaftConfiguration(context.Background()); !store.Alone(cfg.Servers, nodeA) || cfg.Servers[0].Address != store.NoAddress {
		t.Fatalf("raft configuration %+v", cfg)
	}
}

// With a listen address the service opens 8444 on that address only,
// updates both of its own address records, and accepts its own key only.
func TestListenAddressOpensListenerAndUpdatesRecords(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:8444")
	if err != nil {
		t.Skipf("127.0.0.1:8444 is in use on this machine: %v", err)
	}
	probe.Close()
	n, key := oneNode(t, "")
	cfg := clusterconf.Config{NodeID: nodeA, NodeName: "smith", ListenAddress: "127.0.0.1"}
	s := New(Options{Config: cfg, Key: key, Store: n, Interval: 20 * time.Millisecond})
	run(t, s)
	eventually(t, "listener open and records updated", func() bool {
		st := s.Status(context.Background())
		return st.Listening && len(st.Members) == 1 && st.Members[0].Address == "127.0.0.1:8444" &&
			s.Health(context.Background()).Status == models.HealthOK
	})
	rc, _ := s.RaftConfiguration(context.Background())
	if !store.Alone(rc.Servers, nodeA) || rc.Servers[0].Address != "127.0.0.1:8444" {
		t.Fatalf("raft configuration %+v", rc)
	}
	s.mu.Lock()
	bound := s.listener.Addr().String()
	s.mu.Unlock()
	if bound != "127.0.0.1:8444" {
		t.Fatalf("bound %s", bound)
	}
	dial := func(c *tls.Certificate) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", bound,
			&tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{*c}}) // test client
		if err != nil {
			return err
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = conn.Read(make([]byte, 1))
		return err
	}
	if err := dial(key.Certificate()); !errors.Is(err, io.EOF) {
		t.Fatalf("own key: %v", err)
	}
	other, _ := nodekey.Create(t.TempDir(), nodeA, time.Now())
	if err := dial(other.Certificate()); err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "tls:") {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestHealthNamesAKeyThatIsNotPinned(t *testing.T) {
	n, key := oneNode(t, "sha256:"+strings.Repeat("0", 64))
	s := New(Options{Config: clusterconf.Config{NodeID: nodeA, NodeName: "smith"}, Key: key, Store: n})
	h := s.Health(context.Background())
	if h.Status != models.HealthUnavailable || h.Checks["membership"].Status != models.HealthUnavailable {
		t.Fatalf("health %+v", h)
	}
}

func TestHealthNamesAnotherNodeID(t *testing.T) {
	// cluster.json names a node the store does not know.
	n, key := oneNode(t, "")
	other := "11111111-2222-4333-8444-555555555555"
	s := New(Options{Config: clusterconf.Config{NodeID: other, NodeName: "smith"}, Key: key, Store: n})
	if c := s.Health(context.Background()).Checks["membership"]; c.Status != models.HealthUnavailable {
		t.Fatalf("membership check %+v", c)
	}
}

func TestListenerThatCannotBindIsUnhealthy(t *testing.T) {
	n, key := oneNode(t, "")
	// 192.0.2.1 (TEST-NET-1) is not an address of this machine.
	cfg := clusterconf.Config{NodeID: nodeA, NodeName: "smith", ListenAddress: "192.0.2.1"}
	s := New(Options{Config: cfg, Key: key, Store: n, Interval: 20 * time.Millisecond})
	run(t, s)
	eventually(t, "listener check failing", func() bool {
		c := s.Health(context.Background()).Checks["listener"]
		return c.Status == models.HealthUnavailable && strings.Contains(c.Error, "192.0.2.1:8444")
	})
	if s.Status(context.Background()).Listening {
		t.Fatal("status says listening")
	}
}
