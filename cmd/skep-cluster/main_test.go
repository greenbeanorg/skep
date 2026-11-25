package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"unicode"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
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
	// guests is what the fake agent's lists hold; guestErr fails them.
	vms, jails int
	guestErr   error
	// serviceActions records "stop" and "start"; stages records the
	// stage commands run as the service account.
	serviceActions []string
	stages         [][]string
	tty            io.ReadWriteCloser
	onService      func(action string)
	// importState and importErr are what the fake "admin list
	// --import-state" answers ("done" when both are unset).
	importState string
	importErr   error
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
		agentGuests: func(context.Context, string) (int, int, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.vms, w.jails, w.guestErr
		},
		serviceCtl: func(_ context.Context, action string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.serviceActions = append(w.serviceActions, action)
			if w.onService != nil {
				w.onService(action)
			}
			return nil
		},
		runAsAccount: func(ctx context.Context, uid, _ uint32, args []string) ([]byte, error) {
			w.mu.Lock()
			w.stages = append(w.stages, args)
			w.mu.Unlock()
			var out bytes.Buffer
			sub := w.env(int(uid))
			sub.stdout = &out
			err := run(ctx, args, sub)
			return out.Bytes(), err
		},
		hostUsers: func(context.Context) ([]string, error) { return []string{"andy admin", "vera viewer disabled"}, nil },
		hostImportState: func(context.Context) (string, error) {
			if w.importState == "" && w.importErr == nil {
				return "done", nil
			}
			return w.importState, w.importErr
		},
		openTTY: func() (io.ReadWriteCloser, error) {
			if w.tty == nil {
				return nil, errors.New("no terminal")
			}
			return w.tty, nil
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

// D2 of step 8c-2: "init -check" says one word, asks no agent and writes
// nothing; an initialised host says "initialised".
func TestInitCheck(t *testing.T) {
	w := newWorld(t)
	if err := run(context.Background(), []string{"init", "-check", "-data-dir", w.dataDir, "-config", w.config}, w.env(1000)); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("as a user: %v", err)
	}
	if err := run(context.Background(), []string{"init", "-check", "-data-dir", w.dataDir, "-config", w.config}, w.env(0)); err != nil || w.out.String() != "not-initialised\n" {
		t.Fatalf("fresh: %v %q", err, w.out.String())
	}
	if w.agentCalls != 0 || w.dropped {
		t.Fatal("init -check asked the agent or gave up root")
	}
	if _, err := os.Stat(w.dataDir); err == nil {
		t.Fatal("init -check created the data directory")
	}
	if _, err := os.Stat(w.config); err == nil {
		t.Fatal("init -check wrote cluster.json")
	}
	if err := w.init(0); err != nil {
		t.Fatal(err)
	}
	w.out.Reset()
	if err := run(context.Background(), []string{"init", "-check", "-data-dir", w.dataDir, "-config", w.config}, w.env(0)); err != nil || w.out.String() != "initialised\n" {
		t.Fatalf("initialised: %v %q", err, w.out.String())
	}
	// A data directory of another mode is an error (exit 1), not a word.
	w2 := newWorld(t)
	if err := os.MkdirAll(w2.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(w2.dataDir, 0o755)
	if err := run(context.Background(), []string{"init", "-check", "-data-dir", w2.dataDir}, w2.env(0)); err == nil || !strings.Contains(err.Error(), "not 0700") {
		t.Fatalf("mode: %v", err)
	}
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
	n, err := store.Open(store.Options{Dir: layout.State(w.dataDir), NodeID: agentNode})
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
	for _, want := range []string{"STOPPED at log index 7 (applied up to 6)", "unknown to this build"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

// "applied 7 of 10" with only the library's own entries in between is
// shown as up to date, not as a lag (D6 of the 8b-1 run).
func TestStatusDoesNotShowLibraryEntriesAsLag(t *testing.T) {
	for _, c := range []struct {
		upToDate bool
		want     string
	}{
		{true, "up to date, last Skep entry 7 (entries 8-10 are Raft's own)"},
		{false, "applied 7, catching up (Raft applied 10, commit 10)"},
	} {
		w := newWorld(t)
		f := &fakeService{status: localapi.Status{
			Raft:         store.Stats{State: "leader", Term: 2, LastIndex: 10, CommitIndex: 10, AppliedIndex: 10},
			StateMachine: localapi.StateMachineStatus{AppliedIndex: 7, UpToDate: c.upToDate},
		}}
		if err := run(context.Background(), []string{"status", "-socket", f.start(t)}, w.env(os.Getuid())); err != nil {
			t.Fatal(err)
		}
		if out := w.out.String(); !strings.Contains(out, c.want) || strings.Contains(out, "applied 7 of 10") {
			t.Errorf("status output lacks %q:\n%s", c.want, out)
		}
	}
}

func (w *world) listen(euid int, socket string, extra ...string) error {
	args := append([]string{"listen", "-config", w.config, "-data-dir", w.dataDir, "-socket", socket}, extra...)
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
	for _, c := range []string{"recover", "rotate", "feature-level"} {
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

// join-request and join refuse a host with a guest, or whose agent does
// not answer, before they stop anything (decision D1 of the 8b-1 run).
func TestJoinCommandsRefuseAHostWithGuests(t *testing.T) {
	adm := testAdmission(t)
	for _, c := range []struct {
		name       string
		vms, jails int
		err        error
	}{{"a VM", 1, 0, nil}, {"a jail", 0, 1, nil}, {"no agent", 0, 0, errors.New("connection refused")}} {
		for _, cmd := range [][]string{
			{"join-request", "-address", "127.0.0.1"},
			{"join", "-cluster-id", adm.ClusterID, mustAdmissionLine(t, adm)},
		} {
			w := newWorld(t)
			clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
			w.vms, w.jails, w.guestErr = c.vms, c.jails, c.err
			args := append(append([]string{cmd[0]}, "-config", w.config, "-data-dir", w.dataDir, "-socket", filepath.Join(w.dir, "none.sock")), cmd[1:]...)
			err := run(context.Background(), args, w.env(0))
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Errorf("%s with %s: %v", cmd[0], c.name, err)
			}
			if len(w.serviceActions) != 0 || len(w.stages) != 0 {
				t.Errorf("%s with %s: the service was touched: %v %v", cmd[0], c.name, w.serviceActions, w.stages)
			}
		}
	}
}

func testAdmission(t *testing.T) joincode.Admission {
	return joincode.Admission{
		ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10",
		For:       joincode.Node{NodeID: agentNode, Name: "smith", Address: "127.0.0.1:8444", Pin: "sha256:" + strings.Repeat("a", 64)},
		Members:   []joincode.Node{{NodeID: "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22", Name: "langrishe", Address: "10.79.20.82:8444", Pin: "sha256:" + strings.Repeat("b", 64)}},
		ExpiresAt: 1,
	}
}

func mustAdmissionLine(t *testing.T, a joincode.Admission) string {
	l, err := joincode.EncodeAdmission(a)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type fakeTTY struct {
	strings.Reader
	bytes.Buffer
}

func (f *fakeTTY) Read(p []byte) (int, error)  { return f.Reader.Read(p) }
func (f *fakeTTY) Write(p []byte) (int, error) { return f.Buffer.Write(p) }
func (f *fakeTTY) Close() error                { return nil }

// join confirms on the terminal, or with -cluster-id equal to the
// admission's; without either it refuses, and there is no -yes (D4).
func TestJoinConfirmation(t *testing.T) {
	adm := testAdmission(t)
	line := mustAdmissionLine(t, adm)
	for _, c := range []struct {
		name  string
		args  []string
		tty   string
		ok    bool
		cause string
	}{
		{"no terminal, no -cluster-id", nil, "", false, "-cluster-id"},
		{"wrong -cluster-id", []string{"-cluster-id", "11111111-2222-4333-8444-555555555555"}, "", false, "not the admission's cluster"},
		{"terminal says no", nil, "no\n", false, "not confirmed"},
		{"-yes does not exist", []string{"-yes"}, "", false, "not defined"},
	} {
		w := newWorld(t)
		clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
		if c.tty != "" {
			w.tty = &fakeTTY{Reader: *strings.NewReader(c.tty)}
		}
		args := append(append([]string{"join", "-config", w.config, "-data-dir", w.dataDir, "-socket", filepath.Join(w.dir, "none.sock")}, c.args...), line)
		err := run(context.Background(), args, w.env(0))
		if err == nil || !strings.Contains(err.Error()+w.errOut.String(), c.cause) {
			t.Errorf("%s: %v", c.name, err)
		}
		if len(w.serviceActions) != 0 {
			t.Errorf("%s: the service was stopped before the confirmation", c.name)
		}
	}
	// The cluster ID and every member are printed before the question.
	w := newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, line}, w.env(0))
	for _, want := range []string{adm.ClusterID, "langrishe", "10.79.20.82:8444", adm.Members[0].Pin} {
		if !strings.Contains(w.out.String(), want) {
			t.Errorf("the confirmation does not show %q:\n%s", want, w.out.String())
		}
	}
}

// F5 of the 8c-1 review: a host whose import is pending is refused before
// anything is changed; a state that cannot be read lets the join go on and
// says so.
func TestJoinRefusesAPendingImport(t *testing.T) {
	adm := testAdmission(t)
	line := mustAdmissionLine(t, adm)
	w := newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	w.importState = "pending"
	err := run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-cluster-id", adm.ClusterID, line}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), `run "skep-api admin import-users" first`) || len(w.serviceActions) != 0 || len(w.stages) != 0 {
		t.Fatalf("pending: %v %v %v", err, w.serviceActions, w.stages)
	}
	// D18 of step 8c-2: an api.db older than skep-api is refused too.
	w = newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	w.importState = "unmigrated"
	err = run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-cluster-id", adm.ClusterID, line}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "run skep-setup upgrade first") || len(w.serviceActions) != 0 || len(w.stages) != 0 {
		t.Fatalf("unmigrated: %v %v %v", err, w.serviceActions, w.stages)
	}
	w = newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	w.importErr = errors.New("exit status 1")
	w.tty = &fakeTTY{Reader: *strings.NewReader("no\n")}
	err = run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, line}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || !strings.Contains(w.out.String(), "could not be read (exit status 1); the join goes on") {
		t.Fatalf("unreadable: %v\n%s", err, w.out.String())
	}
}

// An admission for another host, a request pasted as an admission, and a
// damaged line are refused before anything happens.
func TestJoinRefusesAWrongAdmission(t *testing.T) {
	adm := testAdmission(t)
	other := adm
	other.For.NodeID = "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33"
	good := mustAdmissionLine(t, adm)
	req, _ := joincode.EncodeRequest(joincode.Request{Node: adm.For})
	for name, line := range map[string]string{
		"for another host": mustAdmissionLine(t, other),
		"a request":        req,
		"damaged":          good[:len(good)-30] + "x" + good[len(good)-29:],
		"truncated":        good[:len(good)/2],
	} {
		w := newWorld(t)
		clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
		err := run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-cluster-id", adm.ClusterID, line}, w.env(0))
		if err == nil || len(w.serviceActions) != 0 || w.agentCalls != 0 {
			t.Errorf("%s: %v (service %v)", name, err, w.serviceActions)
		}
	}
	// Not root: refused before anything.
	w := newWorld(t)
	for _, cmd := range [][]string{{"join", good}, {"join-request", "-address", "127.0.0.1"}, {"admit", "x"}, {"admit", "-withdraw", "x"}} {
		if err := run(context.Background(), cmd, w.env(1000)); err == nil || !strings.Contains(err.Error(), "root") {
			t.Errorf("%v as a user: %v", cmd, err)
		}
	}
	// The stage never runs as root.
	if err := run(context.Background(), []string{stageCommand, "read"}, w.env(0)); err == nil || !strings.Contains(err.Error(), "never as root") {
		t.Errorf("stage as root: %v", err)
	}
}

// toggled is a fake skep_cluster that "service stop" and "start" switch off
// and on, answering status on its socket while on.
type toggled struct {
	t    *testing.T
	path string
	f    *fakeService
	srv  *http.Server
}

func (g *toggled) set(action string) {
	switch action {
	case "stop":
		if g.srv != nil {
			g.srv.Close()
			g.srv = nil
			os.Remove(g.path)
		}
	case "start":
		ln, err := net.Listen("unix", g.path)
		if err != nil {
			g.t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc(localapi.PathStatus, func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(g.f.status) })
		mux.HandleFunc(localapi.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(models.Health{Status: models.HealthOK})
		})
		g.srv = &http.Server{Handler: mux}
		go g.srv.Serve(ln)
	}
}

// The root-run join with a real one-node store: the service is stopped,
// the stage runs as the service account, every handshake fails (the
// member's pin is wrong), the one-node state is put back, the service is
// started again, and nothing in the data directory belongs to root.
func TestJoinThatFailsBeforeStep1PutsTheStateBack(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	key, err := nodekey.Load(w.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	g := &toggled{t: t, path: filepath.Join(w.dir, "c.sock"), f: &fakeService{}}
	g.set("start")
	t.Cleanup(func() { g.set("stop") })
	w.onService = g.set
	adm := testAdmission(t)
	adm.For.Pin = key.Pin()
	adm.Members[0].Address = "127.0.0.3:8444" // nobody listens there
	before, _ := os.ReadFile(filepath.Join(layout.State(w.dataDir), store.DBFile))
	err = run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path,
		"-agent-socket", "/agent.sock", "-cluster-id", adm.ClusterID, mustAdmissionLine(t, adm)}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "join failed") {
		t.Fatalf("join: %v\n%s", err, w.out.String())
	}
	if got := strings.Join(w.serviceActions, ","); got != "stop,start" {
		t.Fatalf("service actions %s", got)
	}
	if len(w.stages) != 1 || w.stages[0][0] != stageCommand || w.stages[0][1] != "join" {
		t.Fatalf("stages %v", w.stages)
	}
	if !strings.Contains(w.out.String(), "one-node cluster is back") {
		t.Fatalf("output:\n%s", w.out.String())
	}
	after, err := os.ReadFile(filepath.Join(layout.State(w.dataDir), store.DBFile))
	if err != nil || len(after) == 0 || len(before) == 0 {
		t.Fatal("the store is not back in place")
	}
	entries, _ := os.ReadDir(w.dataDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "standalone.") || e.Name() == "join.json" {
			t.Fatalf("%s left behind", e.Name())
		}
	}
}

// hasControl reports a control character other than a line break.
func hasControl(s string) bool {
	for _, r := range s {
		if r != '\n' && unicode.IsControl(r) {
			return true
		}
	}
	return false
}

const evil = "\x1b]0;owned\x07\x1b[2J\u009b31m\r"

func TestTermSafe(t *testing.T) {
	if got := termSafe("a" + evil + "b\nc\x7f"); hasControl(got) || strings.Contains(got, "\n") || !strings.HasPrefix(got, "a?]0;owned??[2J?31m?b?c?") {
		t.Fatalf("termSafe: %q", got)
	}
	if got := termSafeLines("one\x1b\ntwo"); got != "one?\ntwo" {
		t.Fatalf("termSafeLines: %q", got)
	}
}

// status prints names, messages and health errors from other nodes with
// their control characters replaced.
func TestStatusReplacesControlCharacters(t *testing.T) {
	w := newWorld(t)
	f := &fakeService{status: localapi.Status{NodeID: agentNode, NodeName: "smith" + evil, ClusterID: "c" + evil, Pin: "p" + evil,
		Build: "b" + evil, AcceptedBy: "state machine" + evil, ListenAddress: "127.0.0.1:8444", Listening: true,
		Join:     &localapi.JoinStatus{Phase: "refused" + evil, ClusterID: "c", Message: "m" + evil},
		Members:  []clusterstate.Member{{ID: "i" + evil, Name: "n" + evil, Address: "a" + evil, Pin: "p" + evil, State: "member" + evil}},
		Pendings: []clusterstate.Pending{{NodeID: "i" + evil, Name: "n" + evil, Address: "a" + evil, Pin: "p" + evil, ExpiresAt: 1}}}}
	if err := run(context.Background(), []string{"status", "-socket", f.start(t)}, w.env(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if hasControl(w.out.String()) || !strings.Contains(w.out.String(), "smith?]0;owned") {
		t.Fatalf("status output carries control characters:\n%q", w.out.String())
	}
}

// join and join-request print the stage's messages and attempts, and
// admit the answer passed from the leader, with control characters
// replaced.
func TestJoinCommandsReplaceControlCharacters(t *testing.T) {
	stageSays := func(w *world, e *env, res stageResult) {
		e.runAsAccount = func(context.Context, uint32, uint32, []string) ([]byte, error) {
			b, _ := json.Marshal(res)
			return b, nil
		}
	}
	// join: a refusal with attempts and a message from other nodes.
	w := newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	g := &toggled{t: t, path: filepath.Join(w.dir, "c.sock"), f: &fakeService{}}
	g.set("start")
	t.Cleanup(func() { g.set("stop") })
	w.onService = g.set
	adm := testAdmission(t)
	e := w.env(0)
	stageSays(w, &e, stageResult{Outcome: "refused", Message: "leader: no" + evil, Attempts: []string{"langrishe: " + evil}, Pin: "p" + evil})
	err := run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path,
		"-cluster-id", adm.ClusterID, mustAdmissionLine(t, adm)}, e)
	if err == nil || hasControl(err.Error()) || hasControl(w.out.String()) || !strings.Contains(w.out.String(), "langrishe: ?]0;owned") {
		t.Fatalf("join: %q\n%q", err, w.out.String())
	}
	// join-request: the stage's refusal.
	w = newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith"}, -1, -1)
	g2 := &toggled{t: t, path: filepath.Join(w.dir, "c.sock"), f: &fakeService{}}
	g2.set("start")
	t.Cleanup(func() { g2.set("stop") })
	w.onService = g2.set
	e = w.env(0)
	stageSays(w, &e, stageResult{Outcome: "not_alone", Message: "two servers" + evil})
	err = run(context.Background(), []string{"join-request", "-address", "127.0.0.1", "-config", w.config, "-data-dir", w.dataDir, "-socket", g2.path}, e)
	if err == nil || hasControl(err.Error()) || !strings.Contains(err.Error(), "two servers?") {
		t.Fatalf("join-request: %q", err)
	}
	// admit: the leader's answer.
	w = newWorld(t)
	path := filepath.Join(t.TempDir(), "a.sock")
	ln, lerr := net.Listen("unix", path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(localapi.PathAdmit, func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(localapi.AdmitResult{Admission: "skep-admission:1:" + evil, OpID: "op" + evil, ExpiresAt: 1})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	req, _ := joincode.EncodeRequest(joincode.Request{Node: adm.For})
	if err := run(context.Background(), []string{"admit", "-socket", path, "-data-dir", w.dataDir, req}, w.env(0)); err != nil {
		t.Fatal(err)
	}
	if hasControl(w.out.String()) || !strings.Contains(w.out.String(), "op?]0;owned") {
		t.Fatalf("admit output carries control characters:\n%q", w.out.String())
	}
}

// admit and join refuse an address whose port is not 8444, before anything
// is sent or changed.
func TestAdmitAndJoinRefuseAnotherPort(t *testing.T) {
	adm := testAdmission(t)
	w := newWorld(t)
	other := adm.For
	other.Address = "127.0.0.1:9444"
	req, _ := joincode.EncodeRequest(joincode.Request{Node: other})
	err := run(context.Background(), []string{"admit", "-socket", filepath.Join(w.dir, "none.sock"), "-data-dir", w.dataDir, req}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "port 9444") {
		t.Fatalf("admit of port 9444: %v", err)
	}
	for name, a := range map[string]joincode.Admission{
		"the admitted node": func() joincode.Admission { x := adm; x.For.Address = "127.0.0.1:9444"; return x }(),
		"a member": func() joincode.Admission {
			x := adm
			x.Members = []joincode.Node{adm.Members[0]}
			x.Members[0].Address = "10.79.20.82:9444"
			return x
		}(),
	} {
		w := newWorld(t)
		clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
		err := run(context.Background(), []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-cluster-id", adm.ClusterID, mustAdmissionLine(t, a)}, w.env(0))
		if err == nil || !strings.Contains(err.Error(), "port 9444") || len(w.serviceActions) != 0 || len(w.stages) != 0 {
			t.Errorf("join with %s on port 9444: %v (service %v)", name, err, w.serviceActions)
		}
	}
}
