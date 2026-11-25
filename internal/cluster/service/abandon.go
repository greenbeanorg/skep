package service

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// AbandonOutcome is the result of StageAbandon. Outcome is one of
// "no_join", "accepted" (a node still accepts this host's key), "no_answer"
// (no node answered), "restored", "stopped" (a test hook) or "error".
type AbandonOutcome struct {
	Outcome    string
	Message    string
	Probes     []string
	AcceptedBy []string
	Standalone string
	// Resumed is true when join.json was already in phase abandoning:
	// the members were not asked again.
	Resumed bool
}

// Crash points of StageAbandon (stop hooks for the tests).
const (
	abandonStopAfterPhase  = "after phase abandoning is written"
	abandonStopAfterDelete = "after the joining state is deleted"
	abandonStopAfterRename = "after the one-node state is back, before join.json is removed"
)

// StageAbandon is the part of "skep-cluster join --abandon" that runs as
// the service account with the service stopped (ADR 0013 B1 step 3.7,
// decision D9 of step 8b-2a). It asks every node of the admission and of
// the stored join answer whether it accepts this host's key; only if at
// least one answers and none accepts, it writes phase "abandoning" to
// join.json and then puts the one-node state back: state/ (the joining
// node's log) is deleted, standalone.<UTC time> is renamed to state/, and
// join.json is removed last. With phase "abandoning" already written it
// finishes those steps without asking again.
func StageAbandon(ctx context.Context, dataDir string, cfg clusterconf.Config, key *nodekey.Key, timeout time.Duration) AbandonOutcome {
	return stageAbandon(ctx, dataDir, cfg, key, timeout, nil)
}

func stageAbandon(ctx context.Context, dataDir string, cfg clusterconf.Config, key *nodekey.Key, timeout time.Duration, stop func(string) bool) AbandonOutcome {
	stopped := func(p string) bool { return stop != nil && stop(p) }
	js, err := joinstate.Read(dataDir)
	if err != nil {
		return AbandonOutcome{Outcome: "error", Message: err.Error()}
	}
	if js == nil {
		return AbandonOutcome{Outcome: "no_join", Message: "this host has no join in progress (no join.json); there is nothing to give up"}
	}
	out := AbandonOutcome{Standalone: js.Standalone}
	if js.Phase == joinstate.PhaseAbandoning {
		out.Resumed = true
	} else {
		answered := 0
		for _, n := range js.AllNodes(cfg.NodeID) {
			res, perr := raftnet.ProbeKey(n.Address, n.Pin, key.Certificate, timeout)
			line := fmt.Sprintf("%s (%s, %s): %s", n.Name, n.NodeID, n.Address, res)
			if perr != nil && res != raftnet.ProbeAccepted {
				line += " (" + perr.Error() + ")"
			}
			out.Probes = append(out.Probes, line)
			switch res {
			case raftnet.ProbeAccepted:
				answered++
				out.AcceptedBy = append(out.AcceptedBy, n.Name)
			case raftnet.ProbeRefused:
				answered++
			}
		}
		switch {
		case len(out.AcceptedBy) > 0:
			out.Outcome = "accepted"
			out.Message = "a node still accepts this host's key; nothing was changed"
			return out
		case answered == 0:
			out.Outcome = "no_answer"
			out.Message = "no node of the admission or of the join answer answered; nothing was changed"
			return out
		}
		js.Phase = joinstate.PhaseAbandoning
		if err := joinstate.Write(dataDir, *js); err != nil {
			return AbandonOutcome{Outcome: "error", Message: "writing phase abandoning failed: " + err.Error() + "; nothing was changed", Probes: out.Probes}
		}
		if stopped(abandonStopAfterPhase) {
			out.Outcome = "stopped"
			return out
		}
	}
	aside := filepath.Join(dataDir, js.Standalone)
	hasAside, err := layout.Exists(aside)
	if err != nil {
		return AbandonOutcome{Outcome: "error", Message: err.Error(), Probes: out.Probes}
	}
	hasState, err := layout.Exists(layout.State(dataDir))
	if err != nil {
		return AbandonOutcome{Outcome: "error", Message: err.Error(), Probes: out.Probes}
	}
	switch {
	case hasAside:
		if hasState {
			if err := layout.RemoveAll(layout.State(dataDir)); err != nil {
				return AbandonOutcome{Outcome: "error", Message: "deleting the joining state failed: " + err.Error() + "; run the command again", Probes: out.Probes}
			}
		}
		if stopped(abandonStopAfterDelete) {
			out.Outcome = "stopped"
			return out
		}
		if err := joinstate.PutBack(dataDir, js.Standalone); err != nil {
			return AbandonOutcome{Outcome: "error", Message: "putting the one-node state back failed: " + err.Error() + "; run the command again", Probes: out.Probes}
		}
		if stopped(abandonStopAfterRename) {
			out.Outcome = "stopped"
			return out
		}
	case !hasState:
		return AbandonOutcome{Outcome: "error", Probes: out.Probes,
			Message: fmt.Sprintf("neither the moved-aside state %s nor %s exists; nothing more can be put back", js.Standalone, layout.State(dataDir))}
	}
	// state/ is the one-node state now (the rename happened in this run or
	// an earlier one): join.json goes last.
	if err := joinstate.Remove(dataDir); err != nil {
		return AbandonOutcome{Outcome: "error", Message: "removing join.json failed: " + err.Error() + "; run the command again", Probes: out.Probes}
	}
	out.Outcome = "restored"
	return out
}

var _ = context.Background
