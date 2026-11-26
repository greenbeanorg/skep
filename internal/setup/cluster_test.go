package setup

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeCluster answers setup's skep-cluster questions.
type fakeCluster struct {
	initialised bool
	checkErr    error
	initErr     error
	build       string
	view        ClusterView
	statusErr   error
	inits       int
	statuses    int
}

func (f *fakeCluster) Build(context.Context) (string, error) {
	if f.build == "" {
		return newBuild.Version + "+" + newBuild.Commit, nil
	}
	return f.build, nil
}

func (f *fakeCluster) Initialised(context.Context) (bool, error) { return f.initialised, f.checkErr }

func (f *fakeCluster) Init(context.Context) error {
	f.inits++
	if f.initErr == nil {
		f.initialised = true
	}
	return f.initErr
}

func (f *fakeCluster) Status(context.Context) (ClusterView, error) {
	f.statuses++
	return f.view, f.statusErr
}

// aloneView is a one-node cluster with a leader.
func aloneView(build string) ClusterView {
	var v ClusterView
	v.NodeID, v.NodeName, v.ClusterID, v.Build, v.Health = "n1", "smith", "c1", build, "ok"
	v.Raft.State, v.Raft.LeaderID = "leader", "n1"
	v.StateMachine.UpToDate, v.AcceptedBy = true, "state machine"
	v.Servers = append(v.Servers, struct {
		ID string `json:"id"`
	}{"n1"})
	v.Members = append(v.Members, struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	}{"n1", "smith", "member"})
	return v
}

// D1: setup's own struct reads what skep-cluster's status prints; every
// tag it uses names a field of localapi.Status.
func TestClusterViewFollowsLocalapiStatus(t *testing.T) {
	st := localapi.Status{NodeID: "n1", NodeName: "smith", ClusterID: "c1", ListenAddress: "", Build: "0.1.0+abc",
		Members:      []clusterstate.Member{{ID: "n1", Name: "smith", State: "member"}},
		Pendings:     []clusterstate.Pending{{NodeID: "n2", Name: "wu"}},
		Join:         &localapi.JoinStatus{Phase: "accepted", ClusterID: "c2"},
		Removed:      &localapi.RemovedStatus{ClusterID: "c3"},
		Rotation:     &localapi.RotationStatus{Phase: "adding"},
		StateMachine: localapi.StateMachineStatus{Stalled: &clusterstate.Stall{Index: 7, Reason: "x"}, UpToDate: true},
		Servers:      []store.Server{{ID: "n1", Address: "192.0.2.1:8444", Voter: true}, {ID: "n2"}},
		AcceptedBy:   "membership file"}
	st.Raft.State, st.Raft.LeaderID = "leader", "n1"
	raw, err := json.Marshal(struct {
		Status localapi.Status `json:"status"`
		Health models.Health   `json:"health"`
	}{st, models.Health{Status: "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := parseClusterStatus(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if v.NodeID != "n1" || v.NodeName != "smith" || v.ClusterID != "c1" || v.Build != "0.1.0+abc" || v.Raft.LeaderID != "n1" ||
		v.Raft.State != "leader" || len(v.Members) != 1 || v.Members[0].State != "member" || len(v.Pendings) != 1 ||
		v.Pendings[0].Name != "wu" || v.Join == nil || v.Join.ClusterID != "c2" || v.Removed == nil || v.Removed.ClusterID != "c3" ||
		v.Rotation == nil || v.Rotation.Phase != "adding" || v.StateMachine.Stalled == nil || v.StateMachine.Stalled.Index != 7 || v.Health != "ok" ||
		!v.StateMachine.UpToDate || len(v.Servers) != 2 || v.Servers[0].ID != "n1" || v.AcceptedBy != "membership file" {
		t.Fatalf("%+v", v)
	}
	// Every top-level tag of the view is a tag of localapi.Status.
	tags := map[string]bool{}
	ls := reflect.TypeOf(localapi.Status{})
	for i := range ls.NumField() {
		tags[strings.Split(ls.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	vt := reflect.TypeOf(ClusterView{})
	for i := range vt.NumField() {
		if tag := strings.Split(vt.Field(i).Tag.Get("json"), ",")[0]; tag != "-" && !tags[tag] {
			t.Errorf("ClusterView reads %q, which localapi.Status does not have", tag)
		}
	}
}

func TestCleanControlAndFormatCharacters(t *testing.T) {
	if got := Clean("a\x1b[31mb\u202ec\u200bd\ne"); got != "a?[31mb?c?d?e" {
		t.Fatalf("%q", got)
	}
}

func TestClusterViewAlone(t *testing.T) {
	v := aloneView("b")
	if !v.Alone() {
		t.Fatal("one member, itself")
	}
	w := v
	w.Pendings = append(w.Pendings, struct {
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
	}{"n2", "wu"})
	if w.Alone() {
		t.Fatal("a pending admission is not alone")
	}
	w = v
	w.NodeID = "other"
	if w.Alone() {
		t.Fatal("the one member is another node")
	}
	// F1 of the 8c-2 review: members show one node, servers three.
	w = v
	w.Servers = append(append(w.Servers[:0:0], w.Servers...), struct {
		ID string `json:"id"`
	}{"n2"}, struct {
		ID string `json:"id"`
	}{"n3"})
	if why, settling := w.NotAlone(); why != "the Raft configuration has 3 servers" || settling {
		t.Fatalf("%q %v", why, settling)
	}
	w = v
	w.AcceptedBy = "membership file"
	if why, settling := w.NotAlone(); !strings.Contains(why, "membership file") || !settling {
		t.Fatalf("%q %v", why, settling)
	}
	w = v
	w.StateMachine.Stalled = &struct {
		Index  uint64 `json:"index"`
		Reason string `json:"reason"`
	}{7, "x"}
	if why, _ := w.NotAlone(); !strings.Contains(why, "stopped at log index 7") {
		t.Fatalf("%q", why)
	}
	w = v
	w.Servers = nil
	if why, _ := w.NotAlone(); why != "the Raft configuration has 0 servers" {
		t.Fatalf("%q", why)
	}
}

// F5 of the 8c-2 review: setup's copy of the node-name rule is
// clusterstate.ValidName's.
func TestNodeNameRuleIsClusterstates(t *testing.T) {
	for _, n := range []string{"smith", "smith.greenbean.org", "a", "a-b", "-a", "a-", ".a", "a b", "a;b", "", "NAME", "x\x1b",
		strings.Repeat("a", 253), strings.Repeat("a", 254), "00000000-0000-4000-8000-000000000001", "ä"} {
		_, shown := commandName(n)
		if shown != clusterstate.ValidName(n) {
			t.Errorf("%q: setup %v, clusterstate %v", n, shown, clusterstate.ValidName(n))
		}
	}
	if n, shown := commandName("a;b"); n != "NAME" || shown {
		t.Fatalf("%q %v", n, shown)
	}
}

// The waits for a leader: a stopped state machine and no leader are
// waited out, then said.
func TestWaitForLeader(t *testing.T) {
	c := &fakeCluster{view: aloneView("b")}
	c.view.Raft.LeaderID = ""
	_, err := waitForLeader(context.Background(), c, 3*time.Second, func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "no leader") || c.statuses != 4 {
		t.Fatalf("%v after %d", err, c.statuses)
	}
	c = &fakeCluster{statusErr: errors.New("down")}
	if _, err := waitForLeader(context.Background(), c, time.Second, func(time.Duration) {}); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatal(err)
	}
	c = &fakeCluster{view: aloneView("b")}
	if v, err := waitForLeader(context.Background(), c, time.Second, func(time.Duration) {}); err != nil || v.NodeID != "n1" {
		t.Fatal(v, err)
	}
}

// errRunner fails every command with err.
type errRunner struct{ err error }

func (r errRunner) Run(context.Context, Cmd) (string, error) { return "", r.err }

// F7 of the 8c-2 review: multi-line output of skep-cluster's and skep-api's
// commands is cleaned line by line and keeps its line breaks.
func TestCommandOutputIsCleanedLineByLine(t *testing.T) {
	if got := CleanLines("one\x1b[31m\r\ntwo‮\n\nthree"); got != "one?[31m\ntwo?\n\nthree" {
		t.Fatalf("%q", got)
	}
	_, err := HostCluster{Root: "/", Runner: errRunner{errors.New("exit status 1: skep-cluster: line one\nline\x1b two")}}.Initialised(context.Background())
	if err == nil || !strings.Contains(err.Error(), "line one\nline? two") {
		t.Fatalf("%q", err)
	}
}
