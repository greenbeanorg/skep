package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// StageOutcome is the result of StageJoin. Outcome is one of the Join*
// results, "not_alone", or "error".
type StageOutcome struct {
	Outcome    string
	Message    string
	Standalone string
	Attempts   []string
	Answer     *JoinAnswer
}

// StageJoin is the part of "skep-cluster join" that runs as the service
// account, with the service stopped (ADR 0013 B1, join command steps 1, 4,
// 5 and 7): it reads "is this node alone" from the store, moves the
// one-node state aside, makes the join call, and puts the state back only
// when it knows join step 1 was not applied.
func StageJoin(ctx context.Context, dataDir string, cfg clusterconf.Config, key *nodekey.Key, adm joincode.Admission, now time.Time, pause time.Duration) StageOutcome {
	return stageJoin(ctx, dataDir, cfg, key, adm, now, pause, nil)
}

// Crash points a test stops the stage at (stageJoin's stop, outcome "stopped").
const (
	stopAfterMoveAside  = "after the move aside, before join.json is written"
	stopAfterJoinRemove = "after join.json is removed, before the put-back"
)

func stageJoin(ctx context.Context, dataDir string, cfg clusterconf.Config, key *nodekey.Key, adm joincode.Admission, now time.Time, pause time.Duration, stop func(point string) bool) StageOutcome {
	stopped := func(point string) bool { return stop != nil && stop(point) }
	if adm.For.NodeID != cfg.NodeID || adm.For.Pin != key.Pin() {
		return StageOutcome{Outcome: "not_alone", Message: fmt.Sprintf("the admission is for the key %s, this host's key is %s", adm.For.Pin, key.Pin())}
	}
	if err := layout.CheckOldLayout(dataDir); err != nil {
		return StageOutcome{Outcome: "error", Message: err.Error()}
	}
	servers, err := store.ReadServers(layout.State(dataDir), cfg.NodeID)
	if err != nil {
		return StageOutcome{Outcome: "error", Message: err.Error()}
	}
	if !store.Alone(servers, cfg.NodeID) {
		return StageOutcome{Outcome: "not_alone", Message: fmt.Sprintf("the Raft configuration has %d servers; only a node that is alone can join", len(servers))}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return StageOutcome{Outcome: "error", Message: err.Error()}
	}
	// One rename (decision D1 of step 8b-2a): either it happened or not.
	standalone, err := joinstate.MoveAside(dataDir, now.UTC().Format("20060102T150405Z"))
	if err != nil {
		return StageOutcome{Outcome: "error", Message: "moving the one-node state aside failed: " + err.Error() + "; " + startRuleSays(dataDir)}
	}
	StageMark(dataDir, audit.EventJoinMovedAside, false, map[string]string{"node_id": cfg.NodeID, "cluster_id": adm.ClusterID, "standalone": standalone})
	if stopped(stopAfterMoveAside) {
		return StageOutcome{Outcome: "stopped", Standalone: standalone}
	}
	js := joinstate.State{Phase: joinstate.PhaseCalling, OpID: opID, Admission: adm, Standalone: standalone}
	if err := joinstate.Write(dataDir, js); err != nil {
		if rerr := joinstate.Remove(dataDir); rerr != nil {
			return StageOutcome{Outcome: "error", Message: fmt.Sprintf("writing join.json failed (%v) and removing it failed (%v); the one-node state is in %s", err, rerr, standalone)}
		}
		if perr := joinstate.PutBack(dataDir, standalone); perr != nil {
			return StageOutcome{Outcome: "error", Message: fmt.Sprintf("writing join.json failed (%v) and putting the one-node state back failed (%v); it is in %s; %s", err, perr, standalone, startRuleSays(dataDir))}
		}
		return StageOutcome{Outcome: "error", Message: "writing join.json failed: " + err.Error()}
	}
	caller := raftnet.Caller{NodeID: cfg.NodeID, Certificate: key.Certificate}
	out := JoinCall(ctx, caller, js.Peers(cfg.NodeID), adm.ClusterID, opID, 3, pause, nil)
	res := StageOutcome{Outcome: out.Result, Message: out.Message, Standalone: standalone, Attempts: out.Attempts, Answer: out.Answer}
	switch out.Result {
	case JoinAccepted:
		js.Phase, js.Answer = joinstate.PhaseAccepted, out.Answer.Members
	case JoinUnknown:
		js.Phase = joinstate.PhaseUnknown
	default:
		// Join step 1 is known not to be applied: every handshake
		// failed and nothing was sent, or the leader refused. join.json
		// goes first, so a crash before the put-back leaves the start rule
		// (StartRule) to do it.
		if err := joinstate.Remove(dataDir); err != nil {
			return StageOutcome{Outcome: "error", Standalone: standalone,
				Message: fmt.Sprintf("the join failed before step 1 (%s), but join.json could not be removed: %v; the one-node state stays in %s", out.Message, err, standalone)}
		}
		if stopped(stopAfterJoinRemove) {
			return StageOutcome{Outcome: "stopped", Standalone: standalone}
		}
		if err := joinstate.PutBack(dataDir, standalone); err != nil {
			return StageOutcome{Outcome: "error", Standalone: standalone,
				Message: fmt.Sprintf("the join failed before step 1 (%s); join.json is removed, but putting the one-node state back failed: %v; it is in %s; %s", out.Message, err, standalone, startRuleSays(dataDir))}
		}
		res.Standalone = ""
		return res
	}
	if err := joinstate.Write(dataDir, js); err != nil {
		res.Message += "; recording the outcome failed: " + err.Error()
	}
	return res
}

// StartRule is the start rule, before anything is opened (decisions D1 and
// D2 of step 8b-2a):
//   - a wipe marker: refuse, "leave --wipe" did not finish;
//   - raft.db directly in the data directory (an earlier build's layout):
//     refuse and name the paths to move;
//   - join.json in phase "abandoning": refuse, "join --abandon" did not
//     finish;
//   - state/ present: start normally;
//   - state/ absent and join.json present: the join decides (the service
//     starts on an empty state);
//   - state/ absent, no join.json, exactly one standalone.*: rename it
//     back, with a warning; more than one: refuse and name them;
//   - nothing: not initialised.
func StartRule(dataDir string, log *slog.Logger) error {
	if ok, err := layout.Exists(filepath.Join(dataDir, layout.WipeMarker)); err != nil || ok {
		if err == nil {
			err = fmt.Errorf("refusing to start: %s exists: \"skep-cluster leave --wipe\" did not finish; run it again as root", filepath.Join(dataDir, layout.WipeMarker))
		}
		return err
	}
	if err := layout.CheckOldLayout(dataDir); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	js, err := joinstate.Read(dataDir)
	if err != nil {
		return fmt.Errorf("the join record: %w", err)
	}
	if js != nil && js.Phase == joinstate.PhaseAbandoning {
		return errors.New("refusing to start: \"skep-cluster join --abandon\" did not finish (join.json is in phase abandoning); run it again as root")
	}
	if ok, err := layout.Exists(layout.State(dataDir)); err != nil || ok {
		return err // present (nil): start normally
	}
	if js != nil {
		return nil // the join decides
	}
	aside, err := layout.Standalones(dataDir)
	if err != nil {
		return err
	}
	switch len(aside) {
	case 0:
		return nil // store.Open reports a host that was never initialised
	case 1:
		if err := joinstate.PutBack(dataDir, aside[0]); err != nil {
			return fmt.Errorf("there is no state and no join.json; putting the one-node state back from %s failed: %w", filepath.Join(dataDir, aside[0]), err)
		}
		log.Warn("no state and no join.json: the one-node state of a join that stopped was put back", "directory", filepath.Join(dataDir, aside[0]))
		return nil
	}
	return fmt.Errorf("refusing to start: no state, no join.json and more than one moved-aside state in %s: %s", dataDir, strings.Join(aside, ", "))
}

// startRuleSays says what the next start of skep_cluster does with the
// data directory as it is now, so that "the next start puts it back" is
// printed only when it is true.
func startRuleSays(dataDir string) string {
	if ok, _ := layout.Exists(filepath.Join(dataDir, layout.WipeMarker)); ok {
		return "the next start of skep_cluster refuses (a wipe did not finish)"
	}
	if ok, _ := layout.Exists(layout.State(dataDir)); ok {
		return "the next start of skep_cluster uses " + layout.State(dataDir)
	}
	if js, _ := joinstate.Read(dataDir); js != nil {
		return "the next start of skep_cluster continues the join recorded in join.json"
	}
	aside, _ := layout.Standalones(dataDir)
	if len(aside) == 1 {
		return "the next start of skep_cluster puts it back"
	}
	return fmt.Sprintf("the next start of skep_cluster refuses (%d moved-aside states)", len(aside))
}
