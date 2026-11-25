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
		StateMachine: localapi.StateMachineStatus{Stalled: &clusterstate.Stall{Index: 7, Reason: "x"}}}
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
		v.Rotation == nil || v.Rotation.Phase != "adding" || v.StateMachine.Stalled == nil || v.StateMachine.Stalled.Index != 7 || v.Health != "ok" {
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
