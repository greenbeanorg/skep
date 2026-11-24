package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/sysaccount"
	"github.com/greenbeanorg/skep/pkg/models"
)

const agentNode = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"

// world is a host for the command tests: a fake root, a fake agent and a
// record of what the command did with its privileges.
type world struct {
	t       *testing.T
	dir     string
	config  string
	dataDir string
	out     bytes.Buffer
	errOut  bytes.Buffer

	mu sync.Mutex
	// dropped is what existed in the data directory when root was given
	// up, and for whom.
	dropped     bool
	dropUID     uint32
	atDrop      []string
	chowned     []string
	agentCalls  int
	agentSocket string
	restarts    int
	onRestart   func()
	agentErr    error
	agentID     string
}

func newWorld(t *testing.T) *world {
	dir := t.TempDir()
	return &world{t: t, dir: dir, config: filepath.Join(dir, "cluster.json"), dataDir: filepath.Join(dir, "db", "cluster"), agentID: agentNode}
}

func (w *world) env(euid int) env {
	me := uint32(os.Getuid())
	return env{
		stdout: &w.out, stderr: &w.errOut, euid: euid,
		lookup: func(name string) (sysaccount.Account, error) {
			switch name {
			case clusterconf.Account, clusterconf.APIAccount:
				// Both are this user: the tests cannot change UID.
				return sysaccount.Account{UID: me, GID: uint32(os.Getgid())}, nil
			}
			return sysaccount.Account{}, errors.New("no such user")
		},
		drop: func(uid, gid uint32) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.dropped, w.dropUID = true, uid
			entries, _ := os.ReadDir(w.dataDir)
			for _, e := range entries {
				w.atDrop = append(w.atDrop, e.Name())
			}
			return nil
		},
		chown: func(path string, uid, gid int) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.chowned = append(w.chowned, path)
			return nil
		},
		owner:       func(fs.FileInfo) (uint32, bool) { return me, true },
		configOwner: func(uint32) (int, int) { return -1, -1 },
		hostname:    func() (string, error) { return "smith", nil },
		localAddrs:  func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil },
		agentNodeID: func(_ context.Context, socket string) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.agentCalls++
			w.agentSocket = socket
			return w.agentID, w.agentErr
		},
		restartService: func(context.Context) error {
			w.mu.Lock()
			f := w.onRestart
			w.restarts++
			w.mu.Unlock()
			if f != nil {
				f()
			}
			return nil
		},
	}
}

func (w *world) init(euid int, extra ...string) error {
	args := append([]string{"init", "-config", w.config, "-data-dir", w.dataDir, "-agent-socket", "/agent.sock"}, extra...)
	return run(context.Background(), args, w.env(euid))
}

func TestInitNeedsRoot(t *testing.T) {
	w := newWorld(t)
	if err := w.init(1000); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("init as a user: %v", err)
	}
	if w.agentCalls != 0 {
		t.Fatal("the agent was asked before the root check")
	}
	if _, err := os.Stat(w.config); err == nil {
		t.Fatal("cluster.json was written")
	}
}

func TestInitCreatesOneNodeClusterWithoutListener(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0); err != nil {
		t.Fatalf("init: %v\n%s", err, w.errOut.String())
	}
	// The node ID is the agent's, read over the agent socket.
	cfg, err := clusterconf.Load(w.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != agentNode || cfg.NodeName != "smith" || cfg.ListenAddress != "" || w.agentSocket != "/agent.sock" {
		t.Fatalf("cluster.json %+v (agent socket %q)", cfg, w.agentSocket)
	}
	if info, _ := os.Stat(w.config); info.Mode().Perm() != 0o640 {
		t.Fatalf("cluster.json mode %04o", info.Mode().Perm())
	}
	// As root it made the directory, gave it to the account, and
	// nothing else: the key and the store came after root was given up.
	if !w.dropped || w.dropUID != uint32(os.Getuid()) {
		t.Fatal("root was not given up for the service account")
	}
	if len(w.atDrop) != 0 {
		t.Fatalf("files existed in the data directory before root was given up: %v", w.atDrop)
	}
	if len(w.chowned) != 1 || w.chowned[0] != w.dataDir {
		t.Fatalf("chowned %v", w.chowned)
	}
	if info, _ := os.Stat(w.dataDir); info.Mode().Perm() != 0o700 {
		t.Fatalf("data directory mode %04o", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(w.dataDir, nodekey.KeyFile)); info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %04o", info.Mode().Perm())
	}
	key, err := nodekey.Load(w.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	out := w.out.String()
	for _, want := range []string{agentNode, key.Pin(), "none (a single host opens no network port)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The store holds one member: this node, with the key's pin.
	n, err := store.Open(store.Options{Dir: w.dataDir, NodeID: agentNode})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	_ = n.Barrier()
	v := n.Machine().View()
	servers, _ := n.Servers()
	n.Close()
	want := clusterstate.Member{ID: agentNode, Name: "smith", Pin: key.Pin(), State: clusterstate.StateMember}
	if v.ClusterID == "" || len(v.Members) != 1 || v.Members[0] != want {
		t.Fatalf("members %+v", v.Members)
	}
	if !store.Alone(servers, agentNode) || servers[0].Address != store.NoAddress {
		t.Fatalf("servers %+v", servers)
	}

	// A second init is refused and changes nothing.
	w.agentID = "11111111-2222-4333-8444-555555555555"
	if err := w.init(0); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second init: %v", err)
	}
	if again, _ := clusterconf.Load(w.config); again != cfg {
		t.Fatal("the refused init rewrote cluster.json")
	}
	if k2, _ := nodekey.Load(w.dataDir); k2.Pin() != key.Pin() {
		t.Fatal("the refused init replaced the key")
	}
}

func TestInitWithAddress(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1", "-name", "founder"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := clusterconf.Load(w.config)
	if cfg.ListenAddress != "127.0.0.1" || cfg.NodeName != "founder" {
		t.Fatalf("cluster.json %+v", cfg)
	}
}

func TestInitRefusals(t *testing.T) {
	cases := map[string]struct {
		args []string
		prep func(*world)
	}{
		"address of another host": {args: []string{"-address", "192.0.2.7"}},
		"unspecified address":     {args: []string{"-address", "0.0.0.0"}},
		"address with a port":     {args: []string{"-address", "127.0.0.1:8444"}},
		"agent down":              {prep: func(w *world) { w.agentErr = errors.New("connection refused") }},
		"agent without a node ID": {prep: func(w *world) { w.agentID = "" }},
		"unknown account":         {args: []string{"-account", "nobody-here"}},
		"extra argument":          {args: []string{"now"}},
	}
	for name, c := range cases {
		w := newWorld(t)
		if c.prep != nil {
			c.prep(w)
		}
		if err := w.init(0, c.args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(w.config); err == nil {
			t.Errorf("%s: cluster.json was written", name)
		}
		if store.Exists(w.dataDir) || nodekey.Exists(w.dataDir) {
			t.Errorf("%s: a key or a store was made", name)
		}
		if w.dropped {
			t.Errorf("%s: went on to the unprivileged part", name)
		}
	}
}

func TestInitRefusesForeignDataDirectory(t *testing.T) {
	w := newWorld(t)
	os.MkdirAll(w.dataDir, 0o700)
	e := w.env(0)
	e.owner = func(fs.FileInfo) (uint32, bool) { return 0, true } // root-owned
	err := run(context.Background(), []string{"init", "-config", w.config, "-data-dir", w.dataDir}, e)
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("init into a root-owned directory: %v", err)
	}
}

func TestServeRefusesRoot(t *testing.T) {
	w := newWorld(t)
	err := run(context.Background(), []string{"-config", w.config, "-data-dir", w.dataDir}, w.env(0))
	if !errors.Is(err, errRoot) {
		t.Fatalf("serve as root: %v", err)
	}
}

func TestServeNeedsInit(t *testing.T) {
	w := newWorld(t)
	err := run(context.Background(), []string{"-config", w.config, "-data-dir", w.dataDir}, w.env(1000))
	if !errors.Is(err, clusterconf.ErrNotInitialised) {
		t.Fatalf("serve before init: %v", err)
	}
}

// Init, then the service twice: the pin, the node ID and the cluster ID
// are the same after a restart, and status and health answer on the
// socket.
func TestServeStatusAcrossRestart(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as an unprivileged user")
	}
	w := newWorld(t)
	if err := w.init(0); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(w.dir, "c.sock")
	serve := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"-config", w.config, "-data-dir", w.dataDir, "-socket", socket, "-log-level", "error"}, w.env(os.Getuid()))
		}()
		return func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	read := func() localapi.Status {
		t.Helper()
		c := localapi.NewClient(socket)
		var st localapi.Status
		var err error
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if st, err = c.Status(context.Background()); err == nil && st.Raft.State == "leader" && len(st.Members) == 1 {
				if h, herr := c.Health(context.Background()); herr == nil && h.Status == "ok" {
					return st
				}
			}
		}
		t.Fatalf("no healthy status: %+v, %v", st, err)
		return st
	}
	stop := serve()
	first := read()
	// The status command prints the same.
	w.out.Reset()
	if err := run(context.Background(), []string{"status", "-socket", socket}, w.env(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{first.NodeID, first.Pin, first.ClusterID, "none (single host, no network port)", "Health:        ok"} {
		if !strings.Contains(w.out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, w.out.String())
		}
	}
	w.out.Reset()
	if err := run(context.Background(), []string{"health", "-socket", socket}, w.env(os.Getuid())); err != nil {
		t.Fatalf("health: %v", err)
	}
	stop()
	if _, err := os.Stat(socket); err == nil {
		t.Error("the socket was left after the service stopped")
	}
	// A node.crt emptied while the service is stopped is issued again with
	// the same key at the next start: same pin, same node ID.
	if err := os.WriteFile(filepath.Join(w.dataDir, nodekey.CertFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stop = serve()
	second := read()
	stop()
	if k, err := nodekey.Load(w.dataDir); err != nil || k.Pin() != first.Pin {
		t.Fatalf("node.crt after the start: %v", err)
	}
	if first.NodeID != agentNode || second.NodeID != first.NodeID || second.Pin != first.Pin || second.ClusterID != first.ClusterID {
		t.Fatalf("identity changed across a restart:\n%+v\n%+v", first, second)
	}
	if first.Listening || second.Listening || second.ListenAddress != "" {
		t.Fatal("a single host listens")
	}
}

// fakeService answers the local API on a plain Unix socket, as if the
// caller were root, with a chosen Raft configuration.
type fakeService struct {
	mu      sync.Mutex
	servers []store.Server
	status  localapi.Status
}

func (f *fakeService) start(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(localapi.PathRaftConfiguration, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(localapi.RaftConfiguration{Servers: f.servers})
	})
	mux.HandleFunc(localapi.PathStatus, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.status)
	})
	mux.HandleFunc(localapi.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(models.Health{Status: models.HealthOK})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// status prints the state machine's applied index, not the library's, and
// says so when the machine is stalled.
func TestStatusPrintsTheStateMachine(t *testing.T) {
	w := newWorld(t)
	f := &fakeService{status: localapi.Status{
		Raft:         store.Stats{State: "leader", Term: 2, LastIndex: 9, AppliedIndex: 9},
		StateMachine: localapi.StateMachineStatus{AppliedIndex: 6, Stalled: &clusterstate.Stall{Index: 7, Reason: "entry type \"x\" is unknown to this build"}},
	}}
	if err := run(context.Background(), []string{"status", "-socket", f.start(t)}, w.env(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	out := w.out.String()
	for _, want := range []string{"applied 6 of 9", "STOPPED at log index 7 (applied up to 6)", "unknown to this build"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

func (w *world) listen(euid int, socket string, extra ...string) error {
	args := append([]string{"listen", "-config", w.config, "-socket", socket}, extra...)
	return run(context.Background(), args, w.env(euid))
}

func TestListenWritesConfigAndRestarts(t *testing.T) {
	w := newWorld(t)
	if err := clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith"}, -1, -1); err != nil {
		t.Fatal(err)
	}
	f := &fakeService{servers: []store.Server{{ID: agentNode, Address: store.NoAddress, Voter: true}}}
	socket := f.start(t)
	w.onRestart = func() {
		// The restarted service reads the new cluster.json.
		cfg, _ := clusterconf.Load(w.config)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.status = localapi.Status{ListenAddress: cfg.PeerAddress(), Listening: true,
			Members: []clusterstate.Member{{ID: agentNode, Address: cfg.PeerAddress()}}}
	}
	if err := w.listen(0, socket, "-address", "127.0.0.1"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg, _ := clusterconf.Load(w.config)
	if cfg.ListenAddress != "127.0.0.1" || cfg.NodeID != agentNode || w.restarts != 1 {
		t.Fatalf("cluster.json %+v, %d restarts", cfg, w.restarts)
	}
	if info, _ := os.Stat(w.config); info.Mode().Perm() != 0o640 {
		t.Fatalf("cluster.json mode %04o", info.Mode().Perm())
	}
	if !strings.Contains(w.out.String(), "127.0.0.1:8444") {
		t.Fatalf("output: %s", w.out.String())
	}
}

func TestListenRefusals(t *testing.T) {
	alone := []store.Server{{ID: agentNode, Address: store.NoAddress, Voter: true}}
	two := append([]store.Server{{ID: "other", Address: "192.0.2.9:8444", Voter: false}}, alone...)
	foreign := []store.Server{{ID: "other", Address: "192.0.2.9:8444", Voter: true}}
	cases := map[string]struct {
		euid    int
		args    []string
		servers []store.Server
	}{
		"not root":                {1000, []string{"-address", "127.0.0.1"}, alone},
		"no address":              {0, nil, alone},
		"address of another host": {0, []string{"-address", "192.0.2.7"}, alone},
		"unspecified address":     {0, []string{"-address", "::"}, alone},
		"two servers":             {0, []string{"-address", "127.0.0.1"}, two},
		"another node alone":      {0, []string{"-address", "127.0.0.1"}, foreign},
	}
	for name, c := range cases {
		w := newWorld(t)
		before := clusterconf.Config{NodeID: agentNode, NodeName: "smith"}
		if err := clusterconf.Write(w.config, before, -1, -1); err != nil {
			t.Fatal(err)
		}
		f := &fakeService{servers: c.servers}
		if err := w.listen(c.euid, f.start(t), c.args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if after, _ := clusterconf.Load(w.config); after != before {
			t.Errorf("%s: cluster.json was changed to %+v", name, after)
		}
		if w.restarts != 0 {
			t.Errorf("%s: the service was restarted", name)
		}
	}
}

func TestListenNeedsTheRunningService(t *testing.T) {
	w := newWorld(t)
	before := clusterconf.Config{NodeID: agentNode, NodeName: "smith"}
	clusterconf.Write(w.config, before, -1, -1)
	err := w.listen(0, filepath.Join(w.dir, "absent.sock"), "-address", "127.0.0.1")
	if !errors.Is(err, localapi.ErrUnavailable) {
		t.Fatalf("listen without the service: %v", err)
	}
	if after, _ := clusterconf.Load(w.config); after != before {
		t.Fatal("cluster.json was changed")
	}
}

func TestUnknownCommand(t *testing.T) {
	w := newWorld(t)
	for _, c := range []string{"join", "admit", "join-request", "recover"} {
		if err := run(context.Background(), []string{c}, w.env(0)); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s: %v", c, err)
		}
	}
}

// The real agent read: over a Unix socket, the read-only system
// operation, with the actor the agent requires.
func TestAgentNodeIDReadsTheSystemOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var gotMethod, gotPath, gotActor string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotActor = r.Method, r.URL.Path, r.Header.Get("Skep-Actor-Id")
		w.Write([]byte(`{"node_id":"` + agentNode + `","hostname":"smith"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	id, err := agentNodeID(context.Background(), path)
	if err != nil || id != agentNode {
		t.Fatalf("agentNodeID = %q, %v", id, err)
	}
	if gotMethod != http.MethodGet || gotPath != "/internal/v1/system" || gotActor != "skep-cluster-init" {
		t.Fatalf("request: %s %s as %q", gotMethod, gotPath, gotActor)
	}
	if _, err := agentNodeID(context.Background(), filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatal("no error without an agent")
	}
}
