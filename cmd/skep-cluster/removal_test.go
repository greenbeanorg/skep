package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/pkg/models"
)

// serviceWithStatus is a fake skep_cluster switched by "service stop" and
// "start" whose status the test sets.
func serviceWithStatus(t *testing.T, w *world, st localapi.Status) *toggled {
	g := &toggled{t: t, path: filepath.Join(w.dir, "c.sock"), f: &fakeService{status: st}}
	g.set("start")
	t.Cleanup(func() { g.set("stop") })
	w.onService = g.set
	return g
}

func readConfig(t *testing.T, path string) clusterconf.Config {
	t.Helper()
	c, err := clusterconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// leave --wipe with a real store: the service is stopped, the stages run
// as the service account, the listen address leaves cluster.json, a new
// key and cluster ID, the service is started again, and nothing in the
// data directory belongs to root. A wrong cluster ID changes nothing.
func TestLeaveWipe(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	oldKey, _ := nodekey.Load(w.dataDir)
	oldID, _, err := service.WipeInfo(w.dataDir, agentNode)
	if err != nil {
		t.Fatal(err)
	}
	g := serviceWithStatus(t, w, localapi.Status{})
	args := func(extra ...string) []string {
		return append([]string{"leave", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}, extra...)
	}
	if err := run(context.Background(), []string{"leave", "-config", w.config}, w.env(0)); err == nil || !strings.Contains(err.Error(), "--wipe") {
		t.Fatalf("leave without --wipe: %v", err)
	}
	if err := run(context.Background(), args("-wipe", "-cluster-id", oldID), w.env(1000)); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("not root: %v", err)
	}
	// A wrong cluster ID, and no terminal without -cluster-id: refused,
	// nothing changed, and the service was not stopped (F2: the cluster
	// ID comes from the files while it runs).
	for _, extra := range [][]string{{"-wipe", "-cluster-id", "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"}, {"-wipe"}} {
		w.serviceActions = nil
		if err := run(context.Background(), args(extra...), w.env(0)); err == nil {
			t.Fatalf("%v: accepted", extra)
		}
		if got := strings.Join(w.serviceActions, ","); got != "" {
			t.Fatalf("%v: service actions %s", extra, got)
		}
		if k, _ := nodekey.Load(w.dataDir); k.Pin() != oldKey.Pin() {
			t.Fatal("the key changed")
		}
		if ok, _ := layout.Exists(filepath.Join(w.dataDir, layout.WipeMarker)); ok {
			t.Fatal("a marker was written")
		}
	}
	w.out.Reset()
	w.serviceActions, w.stages = nil, nil
	if err := run(context.Background(), args("-wipe", "-cluster-id", oldID), w.env(0)); err != nil {
		t.Fatalf("wipe: %v\n%s", err, w.out.String())
	}
	if got := strings.Join(w.serviceActions, ","); got != "stop,start" {
		t.Fatalf("service actions %s", got)
	}
	var actions []string
	for _, s := range w.stages {
		actions = append(actions, s[1])
	}
	if strings.Join(actions, ",") != "wipe-info-files,wipe-info,wipe-begin,wipe-finish" {
		t.Fatalf("stages %v", actions)
	}
	if c := readConfig(t, w.config); c.ListenAddress != "" || c.NodeID != agentNode {
		t.Fatalf("cluster.json after the wipe: %+v", c)
	}
	newKey, _ := nodekey.Load(w.dataDir)
	newID, _, _ := service.WipeInfo(w.dataDir, agentNode)
	if newKey.Pin() == oldKey.Pin() || newID == oldID {
		t.Fatal("the key or the cluster ID did not change")
	}
	out := w.out.String()
	for _, want := range []string{newID, newKey.Pin(), "none (no network port)", "skep-cluster remove smith",
		// F3 (fix round of 8d-2): skep-api follows the new cluster within
		// about a minute; admin create before that answers "already exists".
		"nobody can log in to skep-api until an administrator is created",
		"up to about a minute to follow the new one-node cluster", `When "skep-api admin list" prints no user`,
		`(before that it answers "already exists")`, "skep-api admin create --username NAME"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A wipe stopped after its marker is finished by running the command
// again, without a terminal and without -cluster-id: it does not ask.
func TestLeaveWipeRerunDoesNotAsk(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	m, err := service.WipeBegin(w.dataDir, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	g := serviceWithStatus(t, w, localapi.Status{})
	if err := run(context.Background(), []string{"leave", "-wipe", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}, w.env(0)); err != nil {
		t.Fatalf("rerun: %v\n%s", err, w.out.String())
	}
	if !strings.Contains(w.out.String(), "did not finish; finishing it") {
		t.Fatalf("output:\n%s", w.out.String())
	}
	if id, _, _ := service.WipeInfo(w.dataDir, agentNode); id != m.NewClusterID {
		t.Fatalf("cluster %s, the marker said %s", id, m.NewClusterID)
	}
}

// listen -none: refused with a pending admission (read from the store with
// the service stopped); accepted on a node that is alone with nothing
// pending, the listen address leaves cluster.json.
func TestListenNone(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// A pending admission in the store.
	n, err := store.Open(store.Options{Dir: layout.State(w.dataDir), NodeID: agentNode})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	_, gn := ghostNode(t)
	e, _ := clusterstate.NewEntry(clusterstate.TypePendingCreate, "p-1", "root", agentNode, clusterstate.PendingCreate{
		NodeID: gn.NodeID, Name: gn.Name, Address: gn.Address, Pin: gn.Pin, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if _, err := n.Apply(e); err != nil {
		t.Fatal(err)
	}
	n.Close()
	g := serviceWithStatus(t, w, localapi.Status{})
	args := []string{"listen", "-none", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}
	err = run(context.Background(), args, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("with a pending admission: %v", err)
	}
	if c := readConfig(t, w.config); c.ListenAddress == "" {
		t.Fatal("the listen address was removed")
	}
	if err := run(context.Background(), []string{"listen", "-none", "-address", "127.0.0.1", "-config", w.config}, w.env(0)); err == nil {
		t.Fatal("listen -none with -address")
	}
	// Withdraw it, then -none succeeds.
	n, _ = store.Open(store.Options{Dir: layout.State(w.dataDir), NodeID: agentNode})
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	e, _ = clusterstate.NewEntry(clusterstate.TypePendingWithdraw, "w-1", "root", agentNode, clusterstate.PendingWithdraw{NodeID: gn.NodeID, Name: gn.Name})
	if _, err := n.Apply(e); err != nil {
		t.Fatal(err)
	}
	n.Close()
	g.f.status = localapi.Status{Members: []clusterstate.Member{{ID: agentNode, Name: "smith"}}}
	w.out.Reset()
	if err := run(context.Background(), args, w.env(0)); err != nil {
		t.Fatalf("listen -none: %v\n%s", err, w.out.String())
	}
	if c := readConfig(t, w.config); c.ListenAddress != "" {
		t.Fatalf("cluster.json: %+v", c)
	}
	if !strings.Contains(w.out.String(), "no network port is open") {
		t.Fatalf("output:\n%s", w.out.String())
	}
	w.out.Reset()
	if err := run(context.Background(), args, w.env(0)); err != nil || !strings.Contains(w.out.String(), "nothing to do") {
		t.Fatalf("a second -none: %v %s", err, w.out.String())
	}
}

func ghostNode(t *testing.T) (*nodekey.Key, joincode.Node) {
	t.Helper()
	id := "3e8b6d85-0c3b-4f8b-a03e-6c4a9c3d2f44"
	k, err := nodekey.Create(t.TempDir(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k, joincode.Node{NodeID: id, Name: "ghost", Address: "127.0.0.9:8444", Pin: k.Pin()}
}

// join --abandon on a host without a join in progress: refused, and the
// service was not stopped (F2).
func TestAbandonWithoutAJoin(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	g := serviceWithStatus(t, w, localapi.Status{})
	err := run(context.Background(), []string{"join", "-abandon", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "no join in progress") {
		t.Fatalf("abandon: %v", err)
	}
	if got := strings.Join(w.serviceActions, ","); got != "" {
		t.Fatalf("service actions %s", got)
	}
	if err := run(context.Background(), []string{"join", "-abandon", "x"}, w.env(0)); err == nil {
		t.Fatal("join --abandon with an admission")
	}
}

// removeService answers status and remove on a socket: the member is gone
// after the remove.
type removeService struct {
	mu      sync.Mutex
	removed bool
	got     localapi.RemoveRequest
	answer  localapi.RemoveResult
	errCode string
}

func (f *removeService) start(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "r.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(localapi.PathStatus, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := localapi.Status{NodeID: agentNode, ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10",
			Members: []clusterstate.Member{{ID: agentNode, Name: "smith"}}}
		if !f.removed {
			st.Members = append(st.Members, clusterstate.Member{ID: f.answer.NodeID, Name: "star"})
		}
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc(localapi.PathRemove, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewDecoder(r.Body).Decode(&f.got)
		if f.errCode != "" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(models.ErrorResponse{Error: models.ErrorBody{Code: f.errCode, Message: "refused \x1b[2J here"}})
			return
		}
		f.removed = true
		json.NewEncoder(w).Encode(f.answer)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func TestRemoveCommand(t *testing.T) {
	w := newWorld(t)
	f := &removeService{answer: localapi.RemoveResult{NodeID: "2a098aca-9d7a-4c10-9b79-3850c7acec79", Name: "star\u202e", OpID: "op",
		ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10"}}
	sock := f.start(t)
	if err := run(context.Background(), []string{"remove", "-socket", sock, "star"}, w.env(1000)); err == nil {
		t.Fatal("remove as a user")
	}
	if err := run(context.Background(), []string{"remove", "-socket", sock, "-allow-unanswered", "star"}, w.env(0)); err == nil ||
		!strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("override without a terminal: %v", err)
	}
	if err := run(context.Background(), []string{"remove", "-socket", sock, "-allow-unanswered", "-cluster-id", "1c6f4b63-8a1f-4d69-8e1c-4a2e7a1b0d22", "star"}, w.env(0)); err == nil {
		t.Fatal("override with a wrong cluster ID")
	}
	if f.got.Name != "" {
		t.Fatal("a refused confirmation reached the service")
	}
	w.tty = &fakeTTY{Reader: *strings.NewReader("7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10\n")}
	if err := run(context.Background(), []string{"remove", "-socket", sock, "-data-dir", w.dataDir, "-allow-unanswered", "star"}, w.env(0)); err != nil {
		t.Fatalf("remove: %v\n%s", err, w.out.String())
	}
	if !f.got.AllowUnanswered || f.got.ClusterID != "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10" || f.got.Name != "star" {
		t.Fatalf("request %+v", f.got)
	}
	out := w.out.String()
	if strings.ContainsRune(out, '\u202e') || !strings.Contains(out, "Every member has applied the removal") || !strings.Contains(out, "skep-cluster leave --wipe") {
		t.Fatalf("output:\n%s", out)
	}
	f.errCode, f.removed = "voters_unanswered", false
	err := run(context.Background(), []string{"remove", "-socket", sock, "-data-dir", w.dataDir, "star"}, w.env(0))
	if err == nil || !strings.Contains(err.Error(), "voters_unanswered") {
		t.Fatalf("refusal: %v", err)
	}
}

// D14: an unparseable address is named as such; termSafe replaces Unicode
// format characters; request validation refuses every character termSafe
// replaces in a name (and admit prints through termSafe anyway).
func TestSmallFixes(t *testing.T) {
	err := joincode.CheckPorts(joincode.Node{Name: "x", Address: "10.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "is not ip:port") || strings.Contains(err.Error(), "port 0") {
		t.Fatalf("CheckPorts: %v", err)
	}
	for _, r := range []rune{'\u202e', '\u2066', '\u200b', '\ufeff', '\u00ad'} {
		if got := termSafe("a" + string(r) + "b"); got != "a?b" {
			t.Errorf("termSafe(%U) = %q", r, got)
		}
		_, gn := ghostNode(t)
		gn.Name = "a" + string(r) + "b"
		if _, err := joincode.EncodeRequest(joincode.Request{Node: gn}); err == nil {
			t.Errorf("a request named with %U is valid", r)
		}
	}
}

var _ = os.Getpid
