// Package clustertest runs a real one-node skep-cluster in the test's
// process, with its local socket, for the tests of skep-api (step 8c-1,
// D22: skep-api has no second user backend; its tests use a real
// skep-cluster). It is test tooling: no command imports it.
package clustertest

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/usercopy"
)

// Node is a running one-node cluster.
type Node struct {
	// Socket is the local socket; the test's own UID is the API account.
	Socket string
	// Dir is the data directory (the cluster audit is in Dir/audit).
	Dir string
	// ClusterID is the cluster's ID.
	ClusterID string

	cfg     clusterconf.Config
	key     *nodekey.Key
	mu      sync.Mutex
	stopper func()
}

// Start creates and runs a one-node cluster. It is stopped, and its
// directories removed, at the end of the test.
func Start(tb testing.TB) *Node {
	tb.Helper()
	n, err := New()
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(n.Close)
	return n
}

// New creates and runs a one-node cluster in new temporary directories;
// Close stops it and removes them.
func New() (*Node, error) {
	dir, err := os.MkdirTemp("", "sc")
	if err != nil {
		return nil, err
	}
	id, err := ids.NewUUID()
	if err != nil {
		return nil, err
	}
	// sun_path is short: the socket is in the short temporary directory.
	n := &Node{Socket: filepath.Join(dir, "c.sock"), Dir: filepath.Join(dir, "d"), cfg: clusterconf.Config{NodeID: id, NodeName: "test"}}
	fail := func(err error) (*Node, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.Mkdir(n.Dir, 0o700); err != nil {
		return fail(err)
	}
	if n.key, err = nodekey.Create(n.Dir, id, time.Now()); err != nil {
		return fail(err)
	}
	if n.ClusterID, err = ids.NewUUID(); err != nil {
		return fail(err)
	}
	if err := store.Bootstrap(store.Options{Dir: layout.State(n.Dir), NodeID: id, Quiet: true}, ""); err != nil {
		return fail(err)
	}
	if err := n.start(true); err != nil {
		return fail(err)
	}
	return n, nil
}

// Close stops the node and removes its directories.
func (n *Node) Close() {
	n.Stop()
	os.RemoveAll(filepath.Dir(n.Dir))
}

// Stop stops the service and closes its socket (like stopping
// skep_cluster). It may be called more than once.
func (n *Node) Stop() {
	n.mu.Lock()
	stop := n.stopper
	n.stopper = nil
	n.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Restart starts a stopped node again.
func (n *Node) Restart(tb testing.TB) {
	tb.Helper()
	if err := n.start(false); err != nil {
		tb.Fatal(err)
	}
}

func (n *Node) start(first bool) error {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stateDir := layout.State(n.Dir)
	writer, err := membership.NewWriter(stateDir)
	if err != nil {
		return err
	}
	alog, err := audit.Open(n.Dir, nil)
	if err != nil {
		return err
	}
	if alog.Empty() {
		if err := alog.Started(0, 0); err != nil {
			return err
		}
	}
	machine := clusterstate.New()
	machine.SetMembershipWriter(writer.Write)
	machine.SetAuditWriter(alog)
	svc := service.New(service.Options{Config: n.cfg, Key: n.key, DataDir: n.Dir, Machine: machine, Writer: writer, Logger: log, Audit: alog})
	machine.SetOnApplied(svc.OnApplied)
	machine.SetOnUsers(svc.OnUsersChanged)
	st, err := store.Open(store.Options{Dir: stateDir, NodeID: n.cfg.NodeID, Logger: log, Machine: machine, Quiet: true, FastTimers: true})
	if err != nil {
		alog.Close()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.WaitLeader(ctx); err != nil {
		st.Close()
		alog.Close()
		return err
	}
	if first {
		e, err := clusterstate.NewEntry(clusterstate.TypeClusterInit, "init-"+n.cfg.NodeID, clusterstate.ActorSystem, n.cfg.NodeID,
			clusterstate.ClusterInit{ClusterID: n.ClusterID, Member: clusterstate.Member{ID: n.cfg.NodeID, Name: n.cfg.NodeName,
				Pin: n.key.Pin(), State: clusterstate.StateMember}})
		if err == nil {
			_, err = st.Apply(e)
		}
		if err != nil {
			st.Close()
			alog.Close()
			return err
		}
	}
	svc.SetStore(st, nil)
	uid := uint32(os.Getuid())
	ln, err := localapi.Listen(localapi.ListenOptions{Path: n.Socket, AllowedUIDs: []uint32{0, uid}, MaxConnections: 32, Logger: log})
	if err != nil {
		st.Close()
		alog.Close()
		return err
	}
	srv := &http.Server{Handler: localapi.NewHandler(svc, localapi.HandlerOptions{APIUID: uid, Logger: log}),
		ConnContext: localapi.ConnContext, ReadHeaderTimeout: 10 * time.Second}
	runCtx, stopRun := context.WithCancel(context.Background())
	bg := make(chan struct{})
	go func() { defer close(bg); svc.Run(runCtx) }()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "error", err)
		}
	}()
	n.mu.Lock()
	n.stopper = func() {
		_ = srv.Close()
		_ = ln.Close()
		stopRun()
		<-bg
		_ = st.Close()
		_ = alog.Close()
	}
	n.mu.Unlock()
	return nil
}

// Users is skep-api's user stack on a node: the read copy in db (kept by
// its poll in the background until the test ends), the writer that sends
// user changes through the node, and the store.
type Users struct {
	Store  *apidb.UserStore
	Copy   *usercopy.Copy
	Writer *usercopy.Writer
}

// Users wires skep-api's user stack in db to the node and waits until it
// has quorum. The poll stops at the end of the test.
func (n *Node) Users(tb testing.TB, db *sql.DB) Users {
	tb.Helper()
	u, stop, err := n.UserStack(db)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(stop)
	return u
}

// UserStack is Users without a test: stop ends the poll.
func (n *Node) UserStack(db *sql.DB) (Users, func(), error) {
	store := apidb.NewUserStore(db)
	c := usercopy.New(store, userapi.NewClient(n.Socket), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	stop := func() { cancel(); <-done }
	for deadline := time.Now().Add(20 * time.Second); !c.Quorum(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			stop()
			return Users{}, nil, errors.New("the user copy has no quorum: " + c.Problem())
		}
	}
	return Users{Store: store, Copy: c, Writer: usercopy.NewWriter(c)}, stop, nil
}
