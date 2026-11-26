package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stopped reports whether a run stopped or started any service.
func stoppedAny(h *upgradeHost) bool {
	return slices.ContainsFunc(h.calls, func(c string) bool {
		return strings.HasPrefix(c, "service ") && (strings.HasSuffix(c, " stop") || strings.HasSuffix(c, " start"))
	})
}

func writeRC(t *testing.T, h *upgradeHost, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, "etc/rc.conf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// D10: every refusal comes before anything is stopped.
func TestUpgradeClusterRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*Upgrader, *upgradeHost)
		want  string
	}{
		{"older skep-cluster", func(_ *Upgrader, h *upgradeHost) {
			h.builds[clusterBin] = "skep-cluster 0.1.0 (dd7a8b6, built 2026-11-20T00:00:00Z)"
		}, "skep-cluster is 0.1.0+dd7a8b6, skep-setup is 0.1.1+abc1234"},
		{"no account", func(_ *Upgrader, h *upgradeHost) {
			os.WriteFile(filepath.Join(h.root, "etc/passwd"), []byte("skep:*:409:409::/nonexistent:/usr/sbin/nologin\n"), 0o644)
			os.WriteFile(filepath.Join(h.root, "etc/group"), []byte("skep:*:409:\n"), 0o644)
		}, "skep-cluster account is missing"},
		{"enable NO", func(_ *Upgrader, h *upgradeHost) {
			writeRC(t, h, "skep_agent_enable=\"YES\"\nskep_cluster_enable=\"NO\"\nskep_api_enable=\"YES\"\n")
		}, `skep_cluster_enable is "NO"`},
		{"rc.conf not editable", func(_ *Upgrader, h *upgradeHost) {
			writeRC(t, h, "skep_agent_enable=\"YES\"\nif true; then x=1; fi\n")
		}, "run by hand: sysrc skep_cluster_enable=YES"},
		{"unfinished journal", func(_ *Upgrader, h *upgradeHost) {
			writeRC(t, h, "skep_agent_enable=\"YES\"\n")
			os.MkdirAll(JournalDir(h.root, "20261101T000000Z"), 0o700)
		}, "run skep-setup rollback 20261101T000000Z first"},
		{"data dir", func(_ *Upgrader, h *upgradeHost) {
			writeRC(t, h, "skep_cluster_enable=\"YES\"\nskep_cluster_data_dir=\"/srv/cluster\"\n")
		}, "skep_cluster_data_dir is \"/srv/cluster\""},
		{"socket dir", func(_ *Upgrader, h *upgradeHost) {
			writeRC(t, h, "skep_cluster_enable=\"YES\"\nskep_cluster_socket_dir=\"/tmp/c\"\n")
		}, "skep_cluster_socket_dir"},
		{"init -check fails", func(_ *Upgrader, h *upgradeHost) {
			h.fail[clusterBin+" init -check"] = errors.New("old layout")
		}, "old layout"},
		{"status unreadable", func(_ *Upgrader, h *upgradeHost) {
			h.fail[clusterBin+" status -json"] = errors.New("socket\x1b[2J")
		}, "its status cannot be read (skep-cluster status: socket?[2J)"},
		{"joining", func(_ *Upgrader, h *upgradeHost) {
			h.view.Join = &struct {
				Phase     string `json:"phase"`
				ClusterID string `json:"cluster_id"`
			}{"accepted", "c2"}
		}, "join --abandon"},
		{"rotating", func(_ *Upgrader, h *upgradeHost) {
			h.view.Rotation = &struct {
				Phase string `json:"phase"`
			}{"adding"}
		}, "key rotate --abort"},
		{"removed", func(_ *Upgrader, h *upgradeHost) {
			h.view.Removed = &struct {
				ClusterID string `json:"cluster_id"`
			}{"c1"}
		}, "leave --wipe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, h := newUpgrade(t)
			c.setup(u, h)
			err := u.Upgrade(context.Background())
			var r *UpgradeRefusal
			if !errors.As(err, &r) || r.After != "" || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("%v", err)
			}
			if stoppedAny(h) {
				t.Fatalf("a refusal stopped or started a service: %v", h.calls)
			}
		})
	}
}

// D10: a stopped state machine does not refuse, and neither does an
// initialised skep_cluster that is not running.
func TestUpgradeClusterNotRefused(t *testing.T) {
	u, h := newUpgrade(t)
	h.view.StateMachine.Stalled = &struct {
		Index  uint64 `json:"index"`
		Reason string `json:"reason"`
	}{9, "unknown entry"}
	var out strings.Builder
	u.Out = &out
	// The restarted build applies it.
	h.on["service skep_cluster start"] = func() { h.view.StateMachine.Stalled = nil }
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state machine is stopped at log index 9") {
		t.Fatal(out.String())
	}
	u, h = newUpgrade(t)
	os.Remove(hostPath(h.root, clusterPID))
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
	u.Prober = fakeProber{f}
	out.Reset()
	u.Out = &out
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "initialised but not running") {
		t.Fatal(out.String())
	}
}

// D11: a 0.1 host: no cluster, no enable line, users pending and alone.
// Upgrade initialises after the agent, adds the line with a journal of
// command "upgrade", starts skep_cluster, starts skep_api, imports.
func TestUpgradeFrom01(t *testing.T) {
	u, h := newUpgrade(t)
	h.clusterInit = false
	os.Remove(hostPath(h.root, clusterPID))
	writeRC(t, h, "skep_agent_enable=\"YES\"\nskep_api_enable=\"YES\"\n")
	h.importState = "pending"
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
	u.Prober = fakeProber{f}
	var out strings.Builder
	u.Out = &out
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err, out.String())
	}
	order := []string{"service skep_agent start", clusterBin + " init", "sysrc -f /etc/rc.conf skep_cluster_enable",
		"service skep_cluster start", "service skep_api start", apiBin + " admin import-users"}
	last := -1
	for _, w := range order {
		i := slices.IndexFunc(h.calls[last+1:], func(c string) bool { return c == w || strings.HasPrefix(c, w+" ") || strings.HasPrefix(c, w+"=") })
		if i < 0 {
			t.Fatalf("%q out of order in\n%s", w, strings.Join(h.calls, "\n"))
		}
		last += i + 1
	}
	js, _ := LoadJournals(h.root)
	if len(js) != 1 || js[0].Command != "upgrade" || !js[0].Complete || js[0].Entries[0].Action.Name != "skep_cluster_enable" {
		t.Fatalf("%+v", js)
	}
	if rc, _ := ReadRCConf(h.root); rc.Main.Vals["skep_cluster_enable"] != "YES" {
		t.Fatal("no enable line")
	}
	for _, w := range []string{"the cluster store (/var/db/skep/cluster) is not backed up by this version", "users: import state done"} {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("no %q in\n%s", w, out.String())
		}
	}
	// Again: no new journal, no second import, no init.
	n := len(h.calls)
	u.Agent = seqAgent{builds: []string{"0.1.1+abc1234"}, n: new(int), health: "ok"}
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.calls[n:] {
		if c == clusterBin+" init" || strings.Contains(c, "import-users") || strings.HasPrefix(c, "sysrc") {
			t.Fatalf("second upgrade ran %q", c)
		}
	}
	if js, _ := LoadJournals(h.root); len(js) != 1 {
		t.Fatalf("second journal: %d", len(js))
	}
}

// D13: the import, by the node's place in its cluster.
func TestUpgradeImport(t *testing.T) {
	// Not alone: nothing sent, the steps printed, exit 0.
	u, h := newUpgrade(t)
	h.importState = "pending"
	h.view.Members = append(h.view.Members, struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}{"n2", "langrishe", "member"})
	var out strings.Builder
	u.Out = &out
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(h.calls, func(c string) bool { return strings.Contains(c, "import-users") }) ||
		!strings.Contains(out.String(), "on one node only") || !strings.Contains(out.String(), "--yes") {
		t.Fatalf("%v\n%s", h.calls, out.String())
	}
	// Alone, the import fails: incomplete with the login text; never --yes.
	u, h = newUpgrade(t)
	h.importState, h.importTo = "pending", "pending"
	err := u.Upgrade(context.Background())
	var inc *UpgradeIncomplete
	if !errors.As(err, &inc) || !strings.Contains(err.Error(), "users_import_pending") || !strings.Contains(err.Error(), "logins work from this host's own users") {
		t.Fatalf("%v", err)
	}
	for _, c := range h.calls {
		if strings.Contains(c, "--yes") {
			t.Fatalf("sent --yes: %q", c)
		}
	}
	// Not pending: the state printed, nothing else.
	u, h = newUpgrade(t)
	h.importState = "superseded"
	out.Reset()
	u.Out = &out
	if err := u.Upgrade(context.Background()); err != nil || !strings.Contains(out.String(), "import state superseded") {
		t.Fatal(err, out.String())
	}
}

// D12: failures from the init on are incomplete, start skep_api again and
// say who can log in meanwhile.
func TestUpgradeClusterFailures(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*Upgrader, *upgradeHost)
		want  string
	}{
		{"init fails", func(u *Upgrader, h *upgradeHost) {
			h.clusterInit = false
			os.Remove(hostPath(h.root, clusterPID))
			f := facts()
			f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
			u.Prober = fakeProber{f}
			h.initErr = errors.New("exit status 1")
		}, "skep-cluster init failed and may have changed the host"},
		{"no leader", func(u *Upgrader, h *upgradeHost) {
			h.view.Raft.LeaderID = ""
			u.LeaderWait = 2e9
		}, "skep_cluster did not come up as it"},
		{"cluster start fails", func(_ *Upgrader, h *upgradeHost) {
			h.fail["service skep_cluster start"] = errors.New("exit 1")
		}, "start skep_cluster"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, h := newUpgrade(t)
			c.setup(u, h)
			err := u.Upgrade(context.Background())
			var inc *UpgradeIncomplete
			if !errors.As(err, &inc) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "skep_api was started again") ||
				!strings.Contains(err.Error(), "an enabled administrator can log in, with a degraded session") {
				t.Fatalf("%v", err)
			}
		})
	}
}

// V6: the non-interruptible part's limit is the sum of its bounded waits.
func TestUpgradeUninterruptibleLimit(t *testing.T) {
	u := &Upgrader{PIDWait: 20e9, SocketWait: DefaultSocketWait, LeaderWait: DefaultUpgradeLeaderWait, ImportWait: importWait}
	if got := u.uninterruptibleLimit().String(); got != "18m50s" {
		t.Fatalf("%s", got)
	}
}

// Found on blazanov: right after skep_api's start the state reads
// "unmigrated" until the service has migrated; upgrade waits it out and
// then imports.
func TestUpgradeWaitsOutUnmigrated(t *testing.T) {
	u, h := newUpgrade(t)
	h.importState = "unmigrated"
	reads := 0
	h.on[apiBin+" admin list"] = func() {
		if reads++; reads == 3 {
			h.importState = "pending"
		}
	}
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(h.calls, func(c string) bool { return strings.Contains(c, "import-users") }) || h.importState != "done" {
		t.Fatalf("no import after unmigrated: %v", h.calls)
	}
	// It stays: incomplete, not "nothing to import".
	u, h = newUpgrade(t)
	h.importState = "unmigrated"
	u.ImportWait = 2e9
	err := u.Upgrade(context.Background())
	var inc *UpgradeIncomplete
	if !errors.As(err, &inc) || !strings.Contains(err.Error(), "still older than the installed skep-api") {
		t.Fatalf("%v", err)
	}
}
