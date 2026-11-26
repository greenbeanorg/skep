package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// item returns the plan's item in area with what, or fails.
func findItem(t *testing.T, p Plan, area, what string) Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Area == area && it.What == what {
			return it
		}
	}
	t.Fatalf("no item %s / %s in %+v", area, what, p.Items)
	return Item{}
}

const clusterWhat = "the cluster on this host"

// D5: the cluster item, decided by "init -check".
func TestPlanClusterItem(t *testing.T) {
	self := newBuild.Version + "+" + newBuild.Commit
	running := facts()
	running.Running = map[string]bool{"skep_agent": true}
	for _, c := range []struct {
		name  string
		o     func(*Options)
		f     Facts
		kind  Kind
		hint  string
		inits bool
	}{
		{"initialised", func(o *Options) { o.Cluster = &fakeCluster{initialised: true} }, facts(), OK, "initialised", false},
		{"agent running", func(o *Options) { o.Cluster = &fakeCluster{} }, running, Add, "rollback does not undo", true},
		{"-start", func(o *Options) { o.Cluster, o.Start = &fakeCluster{}, true }, facts(), Add, "", true},
		{"agent not running, no -start", func(o *Options) { o.Cluster = &fakeCluster{} }, facts(), Manual, "-start", false},
		{"cannot ask, check", func(o *Options) { o.ClusterReason = "run skep-setup as root" }, facts(), Warn, "not checked: run skep-setup as root", false},
		{"cannot ask, apply", func(o *Options) { o.ClusterReason, o.Apply = "-root /x", true }, facts(), Refuse, "cannot tell", false},
		{"init -check fails, apply", func(o *Options) { o.Cluster, o.Apply = &fakeCluster{checkErr: errors.New("old layout")}, true }, facts(), Refuse, "old layout", false},
		{"older agent", func(o *Options) {
			o.Cluster, o.Agent, o.SelfBuild = &fakeCluster{}, fakeAgent{build: "0.1.0+old"}, self
		}, running, Refuse, "run skep-setup upgrade", false},
		{"same agent", func(o *Options) {
			o.Cluster, o.Agent, o.SelfBuild = &fakeCluster{}, fakeAgent{build: self}, self
		}, running, Add, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := newBridgeOpts(stockHost(t))
			c.o(&o)
			p := compute(t, o, c.f)
			it := findItem(t, p, "cluster", clusterWhat)
			if it.Kind != c.kind || !strings.Contains(it.Detail, c.hint) {
				t.Fatalf("%+v", it)
			}
			if got := slices.ContainsFunc(p.Actions(), func(a Action) bool { return a.Type == ActClusterInit }); got != c.inits {
				t.Fatalf("cluster-init planned: %v", got)
			}
		})
	}
}

// D6: with -start, the agent starts first, then the cluster is
// initialised, then skep_cluster and skep_api start, then the
// administrator.
func TestPlanStartOrder(t *testing.T) {
	o := newBridgeOpts(stockHost(t))
	o.Cluster, o.Start, o.Admin = &fakeCluster{}, true, "andy"
	var got []string
	for _, a := range compute(t, o, facts()).Actions() {
		if a.Type != ActSysrc && a.Type != ActWrite && a.Type != ActMkdir {
			got = append(got, a.Type+" "+a.Service)
		}
	}
	want := []string{"service-start skep_agent", "cluster-init ", "service-start skep_cluster", "service-start skep_api", "admin-create "}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

// D3: the skep-cluster account, like the skep account.
func TestPlanClusterAccount(t *testing.T) {
	root := withSkep(t, fixture(t, "stock-ufs"), "409")
	it := findItem(t, compute(t, newBridgeOpts(root), facts()), "cluster", "skep-cluster")
	if it.Kind != OK {
		t.Fatalf("%+v", it)
	}
	root = fixture(t, "stock-ufs")
	appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:409:409::/nonexistent:/usr/sbin/nologin\nskep-cluster:*:410:410::/nonexistent:/usr/sbin/nologin\n")
	appendFile(t, filepath.Join(root, "etc/group"), "skep:*:409:\n")
	if it := findItem(t, compute(t, newBridgeOpts(root), facts()), "cluster", "skep-cluster"); it.Kind != Refuse || !strings.Contains(it.Detail, "only one") {
		t.Fatalf("half there: %+v", it)
	}
	root = fixture(t, "stock-ufs")
	appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:409:409::/nonexistent:/usr/sbin/nologin\nskep-cluster:*:1410:1410::/nonexistent:/usr/sbin/nologin\n")
	appendFile(t, filepath.Join(root, "etc/group"), "skep:*:409:\nskep-cluster:*:1410:\n")
	if it := findItem(t, compute(t, newBridgeOpts(root), facts()), "cluster", "skep-cluster"); it.Kind != Warn {
		t.Fatalf("other numbers: %+v", it)
	}
	root = withSkep(t, fixture(t, "stock-ufs"), "409")
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("skep:*:409:409::/nonexistent:/usr/sbin/nologin\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/group"), []byte("skep:*:409:\n"), 0o644)
	if it := findItem(t, compute(t, newBridgeOpts(root), facts()), "cluster", "skep-cluster"); it.Kind != Refuse || !strings.Contains(it.Detail, "reinstall") {
		t.Fatalf("missing: %+v", it)
	}
}

// D7: cluster-init waits for the agent's socket, then runs init; a
// failure is "may have changed", and rollback says it does not undo it.
func TestApplyClusterInit(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	running := facts()
	running.Running = map[string]bool{"skep_agent": true}
	o := newBridgeOpts(root)
	cl := &fakeCluster{}
	o.Cluster = cl
	a := e.applier()
	a.Cluster = cl
	tries := 0
	a.Agent = agentFunc(func() error {
		if tries++; tries < 3 {
			return errors.New("no socket yet")
		}
		return nil
	})
	if _, err := a.Apply(context.Background(), compute(t, o, running)); err != nil {
		t.Fatal(err)
	}
	if cl.inits != 1 || tries != 3 {
		t.Fatalf("inits %d after %d agent tries", cl.inits, tries)
	}
	r, err := Rollback(root, "", e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(r.Kept, func(k string) bool { return strings.Contains(k, "skep-cluster init: rollback does not undo it") }) {
		t.Fatalf("%q", r.Kept)
	}

	// A failed init: incomplete, with init as "may have changed".
	root = stockHost(t)
	e = newApplyEnv(t, root)
	o = newBridgeOpts(root)
	cl = &fakeCluster{initErr: errors.New("exit status 1")}
	o.Cluster = cl
	a = e.applier()
	a.Cluster, a.Agent = cl, fakeAgent{health: "ok"}
	_, err = a.Apply(context.Background(), compute(t, o, running))
	var inc *Incomplete
	if !errors.As(err, &inc) || len(inc.Uncertain) != 1 || !strings.HasPrefix(inc.Uncertain[0], "skep-cluster init") {
		t.Fatalf("%v", err)
	}
	// The agent never answers: the same, after the wait.
	root = stockHost(t)
	e = newApplyEnv(t, root)
	o = newBridgeOpts(root)
	cl = &fakeCluster{}
	o.Cluster = cl
	a = e.applier()
	a.Cluster, a.Agent, a.AgentWait = cl, noAgent, 2*time.Second
	_, err = a.Apply(context.Background(), compute(t, o, running))
	if !errors.As(err, &inc) || cl.inits != 0 || !strings.Contains(err.Error(), "did not answer on its socket within 2s") {
		t.Fatalf("%v (inits %d)", err, cl.inits)
	}
}

// agentFunc is an agent whose Health is f.
type agentFunc func() error

func (f agentFunc) Build(context.Context) (string, error)  { return "", f() }
func (f agentFunc) Health(context.Context) (string, error) { return "ok", f() }

// D8: the administrator waits for skep-cluster's leader; without one the
// error says so and no admin create runs.
func TestApplyAdminWaitsForTheLeader(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	e.runner.onStart = func(name string) {
		if name == "skep_api" {
			os.MkdirAll(filepath.Join(root, "var/run/skep"), 0o755)
			os.WriteFile(filepath.Join(root, apiPID[1:]), []byte("1\n"), 0o644)
			os.MkdirAll(filepath.Join(root, "var/db/skep/api"), 0o755)
			os.WriteFile(filepath.Join(root, "var/db/skep/api/api.db"), nil, 0o600)
		}
	}
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	cl := &fakeCluster{initialised: true, view: aloneView("b")}
	cl.view.Raft.LeaderID = ""
	o.Cluster = cl
	a := e.applier()
	a.Cluster, a.LeaderWait = cl, 2*time.Second
	_, err := a.Apply(context.Background(), compute(t, o, facts()))
	var adm *AdminCreateError
	if !errors.As(err, &adm) || !strings.Contains(err.Error(), "skep-cluster is not ready to write the administrator") || !strings.Contains(err.Error(), "no leader") {
		t.Fatalf("%v", err)
	}
	if slices.ContainsFunc(e.runner.calls, func(c string) bool { return strings.Contains(c, "admin create") }) {
		t.Fatal("admin create ran without a leader")
	}
}

// D15: remove asks skep-cluster whether this node is alone.
func TestRemoveClusterRule(t *testing.T) {
	member := func(v *ClusterView, id, name string) {
		v.Members = append(v.Members, struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}{id, name, "member"})
	}
	for _, c := range []struct {
		name string
		cl   *fakeCluster
		kind Kind
		hint string
	}{
		{"not initialised", &fakeCluster{}, OK, "none"},
		{"alone", &fakeCluster{initialised: true, view: aloneView("b")}, OK, "alone"},
		{"more members", func() *fakeCluster {
			c := &fakeCluster{initialised: true, view: aloneView("b")}
			member(&c.view, "n2", "langrishe")
			return c
		}(), Refuse, "remove it on another member"},
		{"pending", func() *fakeCluster {
			c := &fakeCluster{initialised: true, view: aloneView("b")}
			c.view.Pendings = append(c.view.Pendings, struct {
				NodeID string `json:"node_id"`
				Name   string `json:"name"`
			}{"n9", "wu\x1b"})
			return c
		}(), Refuse, "pending (wu?)"},
		{"joining", func() *fakeCluster {
			c := &fakeCluster{initialised: true, view: aloneView("b")}
			c.view.Join = &struct {
				Phase     string `json:"phase"`
				ClusterID string `json:"cluster_id"`
			}{"accepted", "c2"}
			return c
		}(), Refuse, "join --abandon"},
		{"removed", func() *fakeCluster {
			c := &fakeCluster{initialised: true}
			c.view.Removed = &struct {
				ClusterID string `json:"cluster_id"`
			}{"c1"}
			return c
		}(), OK, "removed"},
		{"no answer", &fakeCluster{initialised: true, statusErr: errors.New("not reachable")}, Refuse, "start skep_cluster so that remove can see whether this node is alone"},
		{"cannot check", &fakeCluster{checkErr: errors.New("bad dir")}, Refuse, "bad dir"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _ := appliedHost(t)
			p := computeRemove(t, RemoveOptions{Root: e.root, Cluster: c.cl}, facts(), e.runner)
			it := findItem(t, p, "cluster", "this node's cluster")
			if it.Kind != c.kind || !strings.Contains(it.Detail, c.hint) {
				t.Fatalf("%+v", it)
			}
			if c.name == "more members" && (len(it.Steps) != 2 || !strings.HasPrefix(it.Steps[0], "skep-cluster remove smith") || !strings.HasPrefix(it.Steps[1], "skep-cluster leave --wipe")) {
				t.Fatalf("steps %q", it.Steps)
			}
		})
	}
	// No way to ask: refused.
	e, _, _ := appliedHost(t)
	p, err := ComputeRemove(context.Background(), RemoveOptions{Root: e.root, ClusterReason: "run as root"}, facts(), e.runner, noAgent)
	if err != nil || !p.Refused() {
		t.Fatalf("%v %+v", err, p.Items)
	}
}

// D17: -purge-data deletes cluster.json (copy in the journal), the cluster
// directory with the rest of /var/db/skep, and the skep-cluster account.
func TestRemovePurgeDataCluster(t *testing.T) {
	e, _, _ := appliedHost(t)
	for f, c := range map[string]string{"usr/local/etc/skep/cluster.json": "{}\n", "var/db/skep/cluster/state/raft.db": "x"} {
		os.MkdirAll(filepath.Dir(filepath.Join(e.root, f)), 0o755)
		os.WriteFile(filepath.Join(e.root, f), []byte(c), 0o640)
	}
	// A symbolic link inside the cluster directory is unlinked, never
	// followed (F8, V7).
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("k"), 0o600)
	os.Symlink(outside, filepath.Join(e.root, "var/db/skep/cluster/link"))
	p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner)
	acts := strings.Join(actions(p), "\n")
	for _, w := range []string{"rm -rf /var/db/skep/cluster", "rm -rf /usr/local/etc/skep/cluster.json", "pw userdel skep-cluster; pw groupdel skep-cluster"} {
		if !strings.Contains(acts, w) {
			t.Fatalf("no %q in\n%s", w, acts)
		}
	}
	if w := findItem(t, p, "data", "--purge-data"); !strings.Contains(w.Detail, "node key") || !strings.Contains(w.Detail, "cluster audit") {
		t.Fatalf("%+v", w)
	}
	j, err := e.remover().Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(e.root, "var/db/skep/cluster")) || exists(filepath.Join(e.root, "usr/local/etc/skep/cluster.json")) {
		t.Fatal("cluster data left")
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "k" {
		t.Fatal("the purge followed a symbolic link out of the cluster directory")
	}
	if !exists(filepath.Join(JournalDir(e.root, j.ID), "files", "usr_local_etc_skep_cluster.json")) {
		t.Fatal("no copy of cluster.json in the journal")
	}
}

// D12: revertHost removes the line an upgrade's journal added.
func TestRevertHostCountsUpgradeJournals(t *testing.T) {
	u, h := newUpgrade(t)
	os.WriteFile(filepath.Join(h.root, "etc/rc.conf"), []byte("skep_agent_enable=\"YES\"\nskep_api_enable=\"YES\"\n"), 0o644)
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	js, _ := LoadJournals(h.root)
	if len(js) != 1 || js[0].Command != "upgrade" || !js[0].Complete {
		t.Fatalf("%+v", js)
	}
	rc, _ := ReadRCConf(h.root)
	b := &builder{o: Options{Root: h.root}, journals: js, rc: rc, rcOK: true}
	b.revertHost()
	if !slices.ContainsFunc(b.plan.Actions(), func(a Action) bool { return a.Name == "skep_cluster_enable" && a.Op == "-x" }) {
		t.Fatalf("%+v", b.plan.Items)
	}
}

// A host an earlier remove disabled: the hint names onestart.
func TestRemoveNoAnswerDisabledNamesOnestart(t *testing.T) {
	e, _, _ := appliedHost(t)
	appendFile(t, filepath.Join(e.root, "etc/rc.conf"), "skep_cluster_enable=\"NO\"\n")
	p := computeRemove(t, RemoveOptions{Root: e.root, Cluster: &fakeCluster{initialised: true, statusErr: errors.New("not reachable")}}, facts(), e.runner)
	if it := findItem(t, p, "cluster", "this node's cluster"); it.Kind != Refuse || !strings.Contains(it.Detail, "service skep_cluster onestart") {
		t.Fatalf("%+v", it)
	}
	e, _, _ = appliedHost(t)
	p = computeRemove(t, RemoveOptions{Root: e.root, Cluster: &fakeCluster{initialised: true, statusErr: errors.New("not reachable")}}, facts(), e.runner)
	if it := findItem(t, p, "cluster", "this node's cluster"); !strings.Contains(it.Detail, "(service skep_cluster start)") {
		t.Fatalf("enabled: %+v", it)
	}
}
