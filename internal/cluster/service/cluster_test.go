package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// clock is one node's clock: the real time plus an offset the test moves.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// tnode is one node of an in-process cluster: real TLS on its own loopback
// address and port 8444, its own data directory, store and service.
type tnode struct {
	name  string
	id    string
	dir   string
	key   *nodekey.Key
	cfg   clusterconf.Config
	clock *clock
	logs  *logBuffer

	svc    *Service
	st     *store.Node
	cancel context.CancelFunc
	done   chan struct{}
	hook   func(step int, id string) bool
}

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	t     *testing.T
	nodes []*tnode
	next  int
}

func newHarness(t *testing.T) *harness {
	if testing.Short() {
		t.Skip("multi-node test")
	}
	h := &harness{t: t, next: 1}
	t.Cleanup(func() {
		for _, n := range h.nodes {
			h.stop(n)
		}
	})
	return h
}

// node makes a host with a key and cluster.json (as init and
// join-request leave it), on 127.0.88.N:8444.
func (h *harness) node(name string) *tnode {
	h.t.Helper()
	id, err := ids.NewUUID()
	if err != nil {
		h.t.Fatal(err)
	}
	dir := h.t.TempDir()
	key, err := nodekey.Create(dir, id, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	ip := "127.0.88." + string(rune('0'+h.next))
	h.next++
	n := &tnode{name: name, id: id, dir: dir, key: key, clock: &clock{}, logs: &logBuffer{},
		cfg: clusterconf.Config{NodeID: id, NodeName: name, ListenAddress: ip}}
	// A one-node cluster, as "skep-cluster init -address" makes it.
	clusterID, _ := ids.NewUUID()
	opts := store.Options{Dir: dir, NodeID: id}
	if err := store.Bootstrap(opts, n.cfg.PeerAddress()); err != nil {
		h.t.Fatal(err)
	}
	st, err := store.Open(opts)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.WaitLeader(ctx); err != nil {
		h.t.Fatal(err)
	}
	e, _ := clusterstate.NewEntry(clusterstate.TypeClusterInit, "init-"+id, clusterstate.ActorSystem, id, clusterstate.ClusterInit{
		ClusterID: clusterID, Member: clusterstate.Member{ID: id, Name: name, Address: n.cfg.PeerAddress(), Pin: key.Pin(), State: clusterstate.StateMember}})
	if _, err := st.Apply(e); err != nil {
		h.t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.nodes = append(h.nodes, n)
	return n
}

// start runs the node as cmd/skep-cluster's serve does.
func (h *harness) start(n *tnode) {
	h.t.Helper()
	log := slog.New(slog.NewTextHandler(io.MultiWriter(n.logs, testWriter{h.t, n.name}), &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := RecoverStandalone(n.dir, log); err != nil {
		h.t.Fatal(err)
	}
	writer, err := membership.NewWriter(n.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	js, err := joinstate.Read(n.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	if js != nil && js.Phase == joinstate.PhaseCalling {
		js.Phase = joinstate.PhaseUnknown
	}
	machine := clusterstate.New()
	machine.SetMembershipWriter(writer.Write)
	svc := New(Options{Config: n.cfg, Key: n.key, DataDir: n.dir, Machine: machine, Writer: writer, Join: js,
		Now: n.clock.now, Logger: log, Interval: 50 * time.Millisecond, LeaderInterval: 300 * time.Millisecond})
	svc.afterJoinStep = n.hook
	machine.SetOnApplied(svc.OnApplied)
	trans, err := raftnet.New(raftnet.Options{LocalAddress: n.cfg.PeerAddress(), Certificate: n.key.Certificate, Trust: svc, Peer: svc, Logger: log})
	if err != nil {
		h.t.Fatal(err)
	}
	st, err := store.Open(store.Options{Dir: n.dir, NodeID: n.id, Logger: log, Machine: machine, Transport: trans, Empty: js != nil})
	if err != nil {
		h.t.Fatal(err)
	}
	svc.SetStore(st, trans)
	ctx, cancel := context.WithCancel(context.Background())
	n.svc, n.st, n.cancel, n.done = svc, st, cancel, make(chan struct{})
	go func() { defer close(n.done); svc.Run(ctx) }()
}

// stop stops the node's service and store (like stopping skep_cluster).
func (h *harness) stop(n *tnode) {
	if n.cancel == nil {
		return
	}
	n.cancel()
	<-n.done
	_ = n.st.Close()
	n.cancel, n.svc, n.st = nil, nil, nil
}

type testWriter struct {
	t    *testing.T
	name string
}

func (w testWriter) Write(p []byte) (int, error) {
	if testing.Verbose() {
		w.t.Logf("[%s] %s", w.name, strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

func (h *harness) eventually(what string, limit time.Duration, ok func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	h.t.Fatalf("timed out after %s waiting for: %s", limit, what)
}

func (n *tnode) request(t *testing.T) string {
	t.Helper()
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: n.id, Name: n.name, Address: n.cfg.PeerAddress(), Pin: n.key.Pin()}})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// admit runs "skep-cluster admit" on member m for node j.
func (h *harness) admit(m, j *tnode) joincode.Admission {
	h.t.Helper()
	res, err := m.svc.Admit(context.Background(), j.request(h.t), "root")
	if err != nil {
		h.t.Fatalf("admit %s on %s: %v", j.name, m.name, err)
	}
	adm, err := joincode.ParseAdmission(res.Admission)
	if err != nil {
		h.t.Fatal(err)
	}
	return adm
}

// join runs "skep-cluster join" on j: stop, the stage, start.
func (h *harness) join(j *tnode, adm joincode.Admission) StageOutcome {
	h.t.Helper()
	h.stop(j)
	out := StageJoin(context.Background(), j.dir, j.cfg, j.key, adm, time.Now(), 100*time.Millisecond)
	h.start(j)
	return out
}

func (h *harness) leader() *tnode {
	for _, n := range h.nodes {
		if n.svc != nil && n.st.IsLeader() {
			return n
		}
	}
	return nil
}

// voters returns the voters in n's latest Raft configuration.
func voters(n *tnode) map[string]bool {
	out := map[string]bool{}
	servers, _ := n.st.Servers()
	for _, s := range servers {
		if s.Voter {
			out[s.ID] = true
		}
	}
	return out
}

func stateOf(n *tnode, id string) string {
	m, ok := n.svc.machine.Member(id)
	if !ok {
		return ""
	}
	return m.State
}

// joined waits until every running node has j as a voter in state member.
func (h *harness) joined(j *tnode, limit time.Duration) {
	h.t.Helper()
	h.eventually(j.name+" is a voter in state member on every running node", limit, func() bool {
		for _, n := range h.nodes {
			if n.svc == nil {
				continue
			}
			if !voters(n)[j.id] || stateOf(n, j.id) != clusterstate.StateMember {
				return false
			}
		}
		// And the joiner has finished its own join (join.json gone),
		// which it does on its next tick after it is a member.
		return j.svc != nil && j.svc.joinState() == nil
	})
}

// cluster of the given names: the first founds it, the others join.
func (h *harness) cluster(names ...string) []*tnode {
	h.t.Helper()
	var out []*tnode
	for i, name := range names {
		n := h.node(name)
		h.start(n)
		if i > 0 {
			adm := h.admit(h.leaderOrFirst(out), n)
			if o := h.join(n, adm); o.Outcome != JoinAccepted {
				h.t.Fatalf("join %s: %+v", name, o)
			}
			h.joined(n, 60*time.Second)
		} else {
			h.eventually(name+" leads its one-node cluster", 20*time.Second, func() bool { return n.st.IsLeader() })
		}
		out = append(out, n)
	}
	return out
}

func (h *harness) leaderOrFirst(nodes []*tnode) *tnode {
	if l := h.leader(); l != nil {
		return l
	}
	return nodes[0]
}

func readMembershipFile(t *testing.T, n *tnode) (*membership.File, os.FileInfo) {
	t.Helper()
	f, err := membership.Read(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(n.dir, membership.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return f, info
}

// Two hosts join into one cluster: both voters, both members, the files
// as amendment 1 says, the join's own state cleaned up.
func TestJoinTwoNodes(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe")
	smith, lang := nodes[0], nodes[1]
	for _, n := range nodes {
		if v := voters(n); !v[smith.id] || !v[lang.id] || len(v) != 2 {
			t.Fatalf("%s voters %v", n.name, v)
		}
		if stateOf(n, smith.id) != clusterstate.StateMember || stateOf(n, lang.id) != clusterstate.StateMember {
			t.Fatalf("%s states", n.name)
		}
		h.eventually(n.name+" membership file names both members", 10*time.Second, func() bool {
			f, _ := membership.Read(n.dir)
			return f != nil && len(f.Members) == 2 && f.Members[0].State == clusterstate.StateMember && f.Members[1].State == clusterstate.StateMember
		})
		f, info := readMembershipFile(t, n)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s membership.json mode %o", n.name, info.Mode().Perm())
		}
		raw, _ := os.ReadFile(filepath.Join(n.dir, membership.FileName))
		if strings.Contains(string(raw), "pending") || strings.Contains(string(raw), "expires") {
			t.Errorf("%s membership.json holds a pending entry: %s", n.name, raw)
		}
		if f.Epoch != 1 {
			t.Errorf("%s epoch %d", n.name, f.Epoch)
		}
	}
	// The joiner finished: no join.json, no moved-aside state, and its
	// keys come from its own state machine.
	h.eventually("join.json and the moved-aside state removed", 10*time.Second, func() bool {
		js, _ := joinstate.Read(lang.dir)
		entries, _ := os.ReadDir(lang.dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), joinstate.StandalonePrefix) {
				return false
			}
		}
		return js == nil
	})
	if src := lang.svc.acceptedNow().source; src != "state machine" {
		t.Fatalf("langrishe accepts keys by the %s", src)
	}
	// Every membership event is one log line with actor, op ID, origin.
	for _, ev := range []string{"pending created", "pending used", "member joining", "non-voter added", "promoted", "cluster membership: member"} {
		if !strings.Contains(smith.logs.String(), ev) {
			t.Errorf("smith's log lacks %q", ev)
		}
	}
	for _, line := range strings.Split(smith.logs.String(), "\n") {
		if strings.Contains(line, "cluster membership:") && (!strings.Contains(line, "actor=") || !strings.Contains(line, "op_id=") || !strings.Contains(line, "origin=")) {
			t.Errorf("membership log line without actor, op_id or origin: %s", line)
		}
	}
	// join-request and join refuse on a node whose configuration has two
	// servers: the stage reads it from the store.
	h.stop(smith)
	servers, err := store.ReadServers(smith.dir, smith.id)
	if err != nil || len(servers) != 2 || store.Alone(servers, smith.id) {
		t.Fatalf("offline read: %v %v", servers, err)
	}
	adm := joincode.Admission{ClusterID: smith.cfg.NodeID, For: joincode.Node{NodeID: smith.id, Name: smith.name, Address: smith.cfg.PeerAddress(), Pin: smith.key.Pin()},
		Members: []joincode.Node{{NodeID: lang.id, Name: lang.name, Address: lang.cfg.PeerAddress(), Pin: lang.key.Pin()}}, ExpiresAt: 1}
	if out := StageJoin(context.Background(), smith.dir, smith.cfg, smith.key, adm, time.Now(), 0); out.Outcome != "not_alone" {
		t.Fatalf("join on a node of two: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(smith.dir, store.DBFile)); err != nil {
		t.Fatal("the refused join moved the store")
	}
}
