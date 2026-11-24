package service

import (
	"context"
	"fmt"
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
	js := joinstate.State{Phase: joinstate.PhaseCalling, OpID: opID, Admission: adm, Standalone: standalone}
	if err := joinstate.Write(dataDir, js); err != nil {
		if perr := joinstate.PutBack(dataDir, standalone, OneNodeState); perr != nil {
			return StageOutcome{Outcome: "error", Message: fmt.Sprintf("writing join.json failed (%v) and putting the one-node state back failed (%v); it is in %s", err, perr, standalone)}
		}
		return StageOutcome{Outcome: "error", Message: "writing join.json failed: " + err.Error()}
	}
	caller := raftnet.Caller{NodeID: cfg.NodeID, Certificate: key.Certificate}
	out := JoinCall(ctx, caller, js.Peers(cfg.NodeID), opID, 3, pause, nil)
	res := StageOutcome{Outcome: out.Result, Message: out.Message, Standalone: standalone, Attempts: out.Attempts, Answer: out.Answer}
	switch out.Result {
	case JoinAccepted:
		js.Phase, js.Answer = joinstate.PhaseAccepted, out.Answer.Members
	case JoinUnknown:
		js.Phase = joinstate.PhaseUnknown
	default:
		// Join step 1 is known not to be applied: every handshake
		// failed and nothing was sent, or the leader refused.
		if err := joinstate.PutBack(dataDir, standalone, OneNodeState); err != nil {
			js.Phase = joinstate.PhaseUnknown
			_ = joinstate.Write(dataDir, js)
			return StageOutcome{Outcome: "error", Standalone: standalone,
				Message: fmt.Sprintf("the join failed before step 1 (%s), but putting the one-node state back failed: %v; it is in %s", out.Message, err, standalone)}
		}
		if err := joinstate.Remove(dataDir); err != nil {
			return StageOutcome{Outcome: "error", Message: "the one-node state is back, but join.json could not be removed: " + err.Error()}
		}
		res.Standalone = ""
		return res
	}
	if err := joinstate.Write(dataDir, js); err != nil {
		res.Message += "; recording the outcome failed: " + err.Error()
	}
	return res
}
