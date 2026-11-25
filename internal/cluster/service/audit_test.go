package service

import (
	"context"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
)

// records reads n's audit and fails the test on a break.
func records(t *testing.T, n *tnode) []audit.Record {
	t.Helper()
	rep, err := audit.Verify(audit.Dir(n.dir))
	if err != nil || rep.Break != nil {
		t.Fatalf("%s: audit verify: %+v %v", n.name, rep.Break, err)
	}
	var out []audit.Record
	// Verify returns a summary; read the records through a second pass.
	out, err = audit.ReadAll(audit.Dir(n.dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != rep.Records {
		t.Fatalf("%s: %d records read, verify counted %d", n.name, len(out), rep.Records)
	}
	return out
}

func entryOps(recs []audit.Record) map[string]audit.Record {
	out := map[string]audit.Record{}
	for _, r := range recs {
		if r.Kind == audit.KindEntry {
			out[r.OpID] = r
		}
	}
	return out
}

// B10 / D4: every node records the cluster's entries when it applies
// them, with the same operation ID, actor and outcome; a restart that
// replays the log writes nothing twice; a node joined later has nothing
// from before its join; the first record of every audit is audit.started.
func TestClusterAudit(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	nodes := h.cluster("smith", "langrishe")
	leader := h.leader()
	// An admission written while two nodes are members.
	ghostLine, _, _ := ghost(t, "ghost", "127.0.88.9")
	res, err := leader.svc.Admit(context.Background(), ghostLine, "root")
	if err != nil {
		t.Fatal(err)
	}
	h.eventually("both audits have the admission", 20*time.Second, func() bool {
		for _, n := range nodes {
			recs, _ := audit.ReadAll(audit.Dir(n.dir))
			if _, ok := entryOps(recs)[res.OpID]; !ok {
				return false
			}
		}
		return true
	})
	for _, n := range nodes {
		recs := records(t, n)
		if recs[0].Kind != audit.KindStarted {
			t.Fatalf("%s: first record %+v", n.name, recs[0])
		}
		r := entryOps(recs)[res.OpID]
		if r.Actor != "root" || r.Outcome != audit.OutcomeApplied || r.EntryType != "pending.create" || r.Name != "ghost" || r.Epoch != 1 {
			t.Fatalf("%s: %+v", n.name, r)
		}
	}
	// A restart replays the log: nothing is written twice.
	x := follower(h)
	before := len(records(t, x))
	h.stop(x)
	h.start(x)
	h.eventually(x.name+" caught up", 20*time.Second, func() bool {
		return x.svc.machine.View().AppliedIndex >= leader.svc.machine.View().AppliedIndex
	})
	time.Sleep(time.Second)
	if after := records(t, x); len(after) != before {
		t.Fatalf("%s: %d records before the restart, %d after (a replay wrote again)", x.name, before, len(after))
	}
	// A node joined later: its audit starts at its join.
	star := h.node("star")
	h.start(star)
	adm := h.admit(leader, star)
	if o := h.join(star, adm); o.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", o)
	}
	h.joined(star, 60*time.Second)
	recs := records(t, star)
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.Kind)
		if r.Kind == audit.KindEntry && r.OpID == res.OpID {
			t.Fatalf("star recorded an entry from before its join: %+v", r)
		}
	}
	has := func(k string) bool {
		for _, x := range kinds {
			if x == k {
				return true
			}
		}
		return false
	}
	if kinds[0] != audit.KindStarted || !has(audit.EventJoinMovedAside) || !has(audit.EventJoined) {
		t.Fatalf("star's audit: %v", kinds)
	}
	// The join's own entries are on every member.
	joinOp := ""
	for _, r := range records(t, leader) {
		if r.EntryType == "member.join" && r.NodeID == star.id {
			joinOp = r.OpID
		}
	}
	if joinOp == "" {
		t.Fatal("the leader did not record star's join")
	}
}

// D4, as found on the lab: a node that was a member of the same cluster
// before (removed, wiped, joined again with the same node ID) receives the
// log's entries, its older member.join among them. Its audit records
// nothing between node.join.started and this join's own member.join.
func TestClusterAuditRejoin(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	x := follower(h)
	h.remove(leader, x.name)
	h.eventually(x.name+" is gone", 60*time.Second, func() bool { _, ok := leader.svc.machine.Member(x.id); return !ok })
	h.stop(x)
	if _, err := WipeBegin(x.dir, x.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := x.cfg
	cfg.ListenAddress = ""
	if _, _, err := WipeFinish(context.Background(), x.dir, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	x.key = loadKey(t, x.dir)
	h.start(x)
	adm := h.admit(leader, x)
	if o := h.join(x, adm); o.Outcome != JoinAccepted {
		t.Fatalf("join again: %+v", o)
	}
	h.joined(x, 60*time.Second)
	recs := records(t, x)
	started := -1
	for i, r := range recs {
		if r.Kind == audit.EventJoinMovedAside {
			started = i
		}
	}
	if started < 0 || started+1 >= len(recs) {
		t.Fatalf("no join mark or nothing after it")
	}
	next := recs[started+1]
	if next.Kind != audit.KindEntry || next.EntryType != "member.join" || next.NodeID != x.id || next.OpID != recs[started].Details["join_op"] {
		t.Fatalf("the first record after the join mark is %+v", next)
	}
}
