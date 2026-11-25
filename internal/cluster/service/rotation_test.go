package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// pinOn is x's pin and next pin as n has applied them.
func pinOn(n, x *tnode) (string, string) {
	m, ok := n.svc.machine.Member(x.id)
	if !ok {
		return "", ""
	}
	return m.Pin, m.NextPin
}

// rotated waits until every running node has x on pin with no next pin.
func (h *harness) rotated(x *tnode, pin string, limit time.Duration) {
	h.t.Helper()
	h.eventually(x.name+" rotated to "+pin+" on every running node", limit, func() bool {
		for _, n := range h.running() {
			if p, next := pinOn(n, x); p != pin || next != "" {
				return false
			}
		}
		return x.svc != nil && x.svc.rotationState() == nil
	})
}

func fileExists(dir, name string) bool {
	ok, _ := layout.Exists(filepath.Join(dir, name))
	return ok
}

// D1: rotation with every member up. Both pins are accepted during it;
// afterwards the new pin on every node, the files gone, the old key refused
// in the handshake, health ok, and the audit records it on every node.
func TestRotationAllUp(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	x := follower(h)
	oldPin := x.key.Pin()
	// A copy of the old key, to probe with afterwards.
	oldCopy := t.TempDir()
	copyFile(t, filepath.Join(x.dir, nodekey.KeyFile), filepath.Join(oldCopy, nodekey.KeyFile))
	copyFile(t, filepath.Join(x.dir, nodekey.CertFile), filepath.Join(oldCopy, nodekey.CertFile))
	oldKey := loadKey(t, oldCopy)

	res, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if res.OldPin != oldPin || res.NewPin == oldPin || res.AddIndex == 0 {
		t.Fatalf("result %+v", res)
	}
	h.rotated(x, res.NewPin, 60*time.Second)
	if fileExists(x.dir, nodekey.NextKeyFile) || fileExists(x.dir, layout.RotateFile) {
		t.Fatal("rotation files left")
	}
	if x.key.Pin() != res.NewPin {
		t.Fatalf("x uses %s", x.key.Pin())
	}
	if pin, err := nodekey.FilePin(x.dir, nodekey.KeyFile); err != nil || pin != res.NewPin {
		t.Fatalf("node.key: %s %v", pin, err)
	}
	for _, n := range h.running() {
		if n == x {
			continue
		}
		// The old key is refused in the handshake by both others.
		if res, _ := raftnet.ProbeKey(n.cfg.PeerAddress(), n.key.Pin(), oldKey.Certificate, 3*time.Second); res != raftnet.ProbeRefused {
			t.Fatalf("%s: the old key: %s", n.name, res)
		}
		if res, _ := raftnet.ProbeKey(n.cfg.PeerAddress(), n.key.Pin(), x.key.Certificate, 3*time.Second); res != raftnet.ProbeAccepted {
			t.Fatalf("%s: the new key: %s", n.name, res)
		}
	}
	h.eventually("health ok everywhere", 30*time.Second, func() bool {
		for _, n := range h.running() {
			if hl := n.svc.Health(context.Background()); hl.Status != "ok" {
				return false
			}
		}
		return true
	})
	// The audit on every node: the add and the done entries; on x the
	// local steps.
	for _, n := range h.running() {
		types := map[string]bool{}
		for _, r := range records(t, n) {
			if r.Kind == audit.KindEntry && r.NodeID == x.id {
				types[r.EntryType] = true
			}
		}
		if !types[clusterstate.TypeMemberKeyAdd] || !types[clusterstate.TypeMemberKeyDone] {
			t.Fatalf("%s audit: %v", n.name, types)
		}
	}
	steps := ""
	for _, r := range records(t, x) {
		if r.Kind == audit.EventKeyRotation {
			steps += r.Details["step"] + " "
		}
	}
	if steps != "started switched done " {
		t.Fatalf("x's local steps: %q", steps)
	}
	// A second rotation of the same node works the same way.
	res2, err := x.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.rotated(x, res2.NewPin, 60*time.Second)
}

// D1: a one-node cluster rotates at once.
func TestRotationOneNode(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("solo")
	n := h.nodes[0]
	res, err := n.svc.Rotate(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	h.rotated(n, res.NewPin, 20*time.Second)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

var _ = strings.Contains
var _ = localapi.RotateResult{}
