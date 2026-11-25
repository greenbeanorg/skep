package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
)

// F2: listen -none is refused without a stop when the running service
// shows another member, a pending admission, a join or a removed node.
func TestListenNoneRefusesWithoutAStop(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	me := clusterstate.Member{ID: agentNode, Name: "smith"}
	for _, c := range []struct {
		st   localapi.Status
		want string
	}{
		{localapi.Status{Members: []clusterstate.Member{me, {ID: "2a098aca-9d7a-4c10-9b79-3850c7acec79", Name: "star"}}}, "the cluster has 2 members"},
		{localapi.Status{Members: []clusterstate.Member{me}, Pendings: []clusterstate.Pending{{Name: "wu"}}}, "pending"},
		{localapi.Status{Members: []clusterstate.Member{me}, Join: &localapi.JoinStatus{Phase: "accepted"}}, "joining"},
		{localapi.Status{Removed: &localapi.RemovedStatus{ClusterID: "c"}}, "removed"},
	} {
		g := serviceWithStatus(t, w, c.st)
		w.serviceActions, w.stages = nil, nil
		err := run(context.Background(), []string{"listen", "-none", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}, w.env(0))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("%s: %v", c.want, err)
		}
		if len(w.serviceActions) != 0 || len(w.stages) != 0 {
			t.Fatalf("%s: service actions %v, stages %v", c.want, w.serviceActions, w.stages)
		}
		if readConfig(t, w.config).ListenAddress == "" {
			t.Fatal("the listen address was removed")
		}
		g.set("stop")
	}
}

// F2 and F3: join --abandon refuses without a stop when no node answers,
// and when join.json names no moved-aside state that exists.
func TestAbandonRefusesWithoutAStop(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	g := serviceWithStatus(t, w, localapi.Status{})
	_, gn := ghostNode(t) // 127.0.0.9:8444: nobody listens
	adm := joincode.Admission{ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10", Members: []joincode.Node{gn}, ExpiresAt: 1,
		For: joincode.Node{NodeID: agentNode, Name: "smith", Address: "127.0.0.1:8444", Pin: "sha256:" + strings.Repeat("a", 64)}}
	if err := os.Mkdir(w.dataDir+"/standalone.20260101T000000Z", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ standalone, want string }{
		{"standalone.20260101T000000Z", "no node of the admission"},
		{"standalone.20250101T000000Z", "the one-node state is gone"},
	} {
		js := joinstate.State{Phase: joinstate.PhaseRefused, OpID: "2c3e4f50-6a7b-4c8d-9e0f-112233445566", Admission: adm, Standalone: c.standalone}
		if err := joinstate.Write(w.dataDir, js); err != nil {
			t.Fatal(err)
		}
		w.serviceActions = nil
		w.out.Reset()
		start := time.Now()
		err := run(context.Background(), []string{"join", "-abandon", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path}, w.env(0))
		if err == nil || !strings.Contains(err.Error()+w.out.String(), c.want) {
			t.Fatalf("%s: %v\n%s", c.standalone, err, w.out.String())
		}
		if len(w.serviceActions) != 0 {
			t.Fatalf("%s: service actions %v", c.standalone, w.serviceActions)
		}
		if got, _ := joinstate.Read(w.dataDir); got == nil || got.Phase != joinstate.PhaseRefused {
			t.Fatalf("%s: join.json changed", c.standalone)
		}
		t.Logf("%s refused in %s", c.standalone, time.Since(start).Round(time.Millisecond))
	}
}

// F7: status on a removed node that is joining says to abandon the join.
func TestStatusOfARemovedJoiningNode(t *testing.T) {
	for _, joining := range []bool{true, false} {
		w := newWorld(t)
		st := localapi.Status{NodeID: agentNode, Removed: &localapi.RemovedStatus{ClusterID: "c", How: "h", At: "t"}}
		if joining {
			st.Join = &localapi.JoinStatus{Phase: "accepted"}
		}
		f := &fakeService{status: st}
		if err := run(context.Background(), []string{"status", "-socket", f.start(t)}, w.env(os.Getuid())); err != nil {
			t.Fatal(err)
		}
		out := w.out.String()
		if strings.Contains(out, "join --abandon") != joining || strings.Contains(out, "leave --wipe") == joining {
			t.Fatalf("joining %v:\n%s", joining, out)
		}
	}
}
