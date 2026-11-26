package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/pkg/models"
)

// D28 of step 8c-2 against a real one-node skep-cluster behind the API:
// show; admit refused without a listen address, naming the command;
// remove asks for the cluster ID on the terminal and refuses a wrong one
// before anything is sent; with --cluster-id a wrong one is the server's
// cluster_id_mismatch; the viewer cannot admit.
func TestClusterCommands(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "andy")
	out, _, err := skepctl(stateDir, nil, "", "cluster", "show")
	if err != nil || !strings.Contains(out, "Members (1):") || !strings.Contains(out, "voter") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	jout, _, err := skepctl(stateDir, nil, "", "cluster", "show", "--json")
	var c models.Cluster
	if err != nil || json.Unmarshal([]byte(jout), &c) != nil || c.ClusterID == "" {
		t.Fatalf("show --json: %v\n%s", err, jout)
	}
	line, _ := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33",
		Name: "wu", Address: "192.0.2.9:8444", Pin: "sha256:" + strings.Repeat("ab", 32)}})
	if _, _, err := skepctl(stateDir, nil, "", "cluster", "admit", line); err == nil || !strings.Contains(err.Error(), "skep-cluster listen -address ADDR") ||
		!strings.Contains(err.Error(), "cluster_refused") {
		t.Fatalf("admit: %v", err)
	}
	term := &terminal{lines: []string{"not-the-id"}}
	if out, _, err := skepctl(stateDir, term, "", "cluster", "remove", "test"); err == nil || !strings.Contains(err.Error(), "nothing was sent") ||
		!strings.Contains(out, "Remove test (") {
		t.Fatalf("remove, wrong ID typed: %v\n%s", err, out)
	}
	if _, _, err := skepctl(stateDir, &terminal{}, "", "cluster", "remove", "test"); err == nil || !strings.Contains(err.Error(), "--cluster-id") {
		t.Fatalf("remove without a terminal: %v", err)
	}
	if _, _, err := skepctl(stateDir, nil, "", "cluster", "remove", "test", "--cluster-id", "00000000-0000-4000-8000-000000000009"); err == nil ||
		!strings.Contains(err.Error(), "cluster_id_mismatch") {
		t.Fatalf("remove, wrong --cluster-id: %v", err)
	}
	if _, _, err := skepctl(stateDir, nil, "", "cluster", "remove", "nobody", "--cluster-id", c.ClusterID); err == nil {
		t.Fatal("removed a member that does not exist")
	}
	viewer := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, viewer, "vera")
	if _, _, err := skepctl(viewer, nil, "", "cluster", "show"); err != nil {
		t.Fatalf("viewer show: %v", err)
	}
	if _, _, err := skepctl(viewer, nil, "", "cluster", "admit", line); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("viewer admit: %v", err)
	}
}

func TestClusterPrintCleans(t *testing.T) {
	var b strings.Builder
	printCluster(&b, models.Cluster{ClusterID: "c\x1b[2J", NodeName: "n\u202e", Members: []models.ClusterMember{{Name: "a\nb"}}})
	if strings.ContainsAny(b.String(), "\x1b‮") || strings.Contains(b.String(), "a\nb") {
		t.Fatalf("%q", b.String())
	}
}
