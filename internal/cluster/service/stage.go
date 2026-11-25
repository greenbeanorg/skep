package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// OneNodeState is what a join moves aside: the store, the snapshots and
// the applied-membership file (ADR 0013 B1 step 3.4, amendment 1).
var OneNodeState = []string{store.DBFile, "snapshots", membership.FileName}

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
	servers, err := store.ReadServers(dataDir, cfg.NodeID)
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
	standalone, err := joinstate.MoveAside(dataDir, OneNodeState, now.UTC().Format("20060102T150405Z"))
	if err != nil {
		if standalone != "" {
			if perr := joinstate.PutBack(dataDir, standalone, OneNodeState); perr != nil {
				return StageOutcome{Outcome: "error", Message: fmt.Sprintf("moving the one-node state aside failed (%v) and putting it back failed too (%v); it is in %s", err, perr, standalone)}
			}
		}
		return StageOutcome{Outcome: "error", Message: "moving the one-node state aside failed: " + err.Error()}
	}
	if stopped(stopAfterMoveAside) {
		return StageOutcome{Outcome: "stopped", Standalone: standalone}
	}
	js := joinstate.State{Phase: joinstate.PhaseCalling, OpID: opID, Admission: adm, Standalone: standalone}
	if err := joinstate.Write(dataDir, js); err != nil {
		if rerr := joinstate.Remove(dataDir); rerr != nil {
			return StageOutcome{Outcome: "error", Message: fmt.Sprintf("writing join.json failed (%v) and removing it failed (%v); the one-node state is in %s", err, rerr, standalone)}
		}
		if perr := joinstate.PutBack(dataDir, standalone, OneNodeState); perr != nil {
			return StageOutcome{Outcome: "error", Message: fmt.Sprintf("writing join.json failed (%v) and putting the one-node state back failed (%v); it is in %s", err, perr, standalone)}
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
		// (RecoverStandalone) to do it.
		if err := joinstate.Remove(dataDir); err != nil {
			return StageOutcome{Outcome: "error", Standalone: standalone,
				Message: fmt.Sprintf("the join failed before step 1 (%s), but join.json could not be removed: %v; the one-node state stays in %s", out.Message, err, standalone)}
		}
		if stopped(stopAfterJoinRemove) {
			return StageOutcome{Outcome: "stopped", Standalone: standalone}
		}
		if err := joinstate.PutBack(dataDir, standalone, OneNodeState); err != nil {
			return StageOutcome{Outcome: "error", Standalone: standalone,
				Message: fmt.Sprintf("the join failed before step 1 (%s); join.json is removed, but putting the one-node state back failed: %v; it is in %s, and the next start of skep_cluster puts it back", out.Message, err, standalone)}
		}
		res.Standalone = ""
		return res
	}
	if err := joinstate.Write(dataDir, js); err != nil {
		res.Message += "; recording the outcome failed: " + err.Error()
	}
	return res
}

// RecoverStandalone is the start rule, before the store is opened: with no
// join.json and no raft.db, one standalone.* is put back; more than one is
// refused.
func RecoverStandalone(dataDir string, log *slog.Logger) error {
	for _, name := range []string{joinstate.FileName, store.DBFile} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, fs.ErrNotExist) {
			return err // present (nil): nothing to do
		}
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	var aside []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), joinstate.StandalonePrefix) {
			aside = append(aside, e.Name())
		}
	}
	switch len(aside) {
	case 0:
		return nil
	case 1:
		if err := joinstate.PutBack(dataDir, aside[0], OneNodeState); err != nil {
			return fmt.Errorf("there is no store and no join.json; putting the one-node state back from %s failed: %w", filepath.Join(dataDir, aside[0]), err)
		}
		log.Warn("no store and no join.json: the one-node state of a join that stopped was put back", "directory", filepath.Join(dataDir, aside[0]))
		return nil
	}
	return fmt.Errorf("refusing to start: no store, no join.json and more than one moved-aside state in %s: %s", dataDir, strings.Join(aside, ", "))
}
