package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// oneNodeState is what a join moves aside: the store, the snapshots and
// the applied-membership file (ADR 0013 B1 step 3.4, amendment 1).
var oneNodeState = []string{store.DBFile, "snapshots", membership.FileName}

// runStageCmd is the hidden command a root-run command starts as the
// service account. It refuses to run as root. It prints one JSON line.
func runStageCmd(ctx context.Context, args []string, e env) error {
	if e.euid == 0 {
		return errors.New("the stage runs as the service account, never as root")
	}
	if len(args) == 0 {
		return errors.New("stage: read or join")
	}
	flags := flag.NewFlagSet("skep-cluster "+stageCommand, flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		configPath = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir    = flags.String("data-dir", clusterconf.DefaultDataDir, "data directory")
		admission  = flags.String("admission", "", "the admission line (join)")
	)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	var res stageResult
	switch args[0] {
	case "read":
		res = stageRead(*configPath, *dataDir)
	case "join":
		res = stageJoin(ctx, e, *configPath, *dataDir, *admission)
	default:
		return fmt.Errorf("stage: unknown action %q", args[0])
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, string(b))
	return nil
}

func fail(msg string) stageResult { return stageResult{Outcome: "error", Message: msg} }

// readAlone loads the key and reads the Raft configuration from the store.
func readAlone(configPath, dataDir string) (clusterconf.Config, *nodekey.Key, int, bool, error) {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return cfg, nil, 0, false, err
	}
	key, err := nodekey.Load(dataDir)
	if err != nil {
		return cfg, nil, 0, false, err
	}
	servers, err := store.ReadServers(dataDir, cfg.NodeID)
	if err != nil {
		return cfg, key, 0, false, err
	}
	return cfg, key, len(servers), store.Alone(servers, cfg.NodeID), nil
}

func stageRead(configPath, dataDir string) stageResult {
	_, key, n, alone, err := readAlone(configPath, dataDir)
	if err != nil {
		return fail(err.Error())
	}
	if !alone {
		return stageResult{Outcome: "not_alone", Servers: n, Pin: key.Pin(),
			Message: fmt.Sprintf("the Raft configuration has %d servers; only a node that is alone can request to join", n)}
	}
	return stageResult{Outcome: "ok", Servers: n, Pin: key.Pin()}
}

// stageJoin moves the one-node state aside and makes the join call (ADR
// 0013 B1, join command steps 1, 4, 5 and 7).
func stageJoin(ctx context.Context, e env, configPath, dataDir, line string) stageResult {
	adm, err := joincode.ParseAdmission(line)
	if err != nil {
		return fail(err.Error())
	}
	cfg, key, n, alone, err := readAlone(configPath, dataDir)
	if err != nil {
		return fail(err.Error())
	}
	if adm.For.NodeID != cfg.NodeID || adm.For.Pin != key.Pin() {
		return stageResult{Outcome: "not_alone", Pin: key.Pin(),
			Message: fmt.Sprintf("the admission is for the key %s, this host's key is %s", adm.For.Pin, key.Pin())}
	}
	if !alone {
		return stageResult{Outcome: "not_alone", Servers: n, Pin: key.Pin(),
			Message: fmt.Sprintf("the Raft configuration has %d servers; only a node that is alone can join", n)}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return fail(err.Error())
	}
	standalone, err := joinstate.MoveAside(dataDir, oneNodeState, time.Now().UTC().Format("20060102T150405Z"))
	if err != nil {
		if standalone != "" {
			if perr := joinstate.PutBack(dataDir, standalone, oneNodeState); perr != nil {
				return fail(fmt.Sprintf("moving the one-node state aside failed (%v) and putting it back failed too (%v); it is in %s", err, perr, standalone))
			}
		}
		return fail("moving the one-node state aside failed: " + err.Error())
	}
	js := joinstate.State{Phase: joinstate.PhaseCalling, OpID: opID, Admission: adm, Standalone: standalone}
	if err := joinstate.Write(dataDir, js); err != nil {
		if perr := joinstate.PutBack(dataDir, standalone, oneNodeState); perr != nil {
			return fail(fmt.Sprintf("writing join.json failed (%v) and putting the one-node state back failed (%v); it is in %s", err, perr, standalone))
		}
		return fail("writing join.json failed: " + err.Error())
	}
	caller := raftnet.Caller{NodeID: cfg.NodeID, Certificate: key.Certificate}
	out := service.JoinCall(ctx, caller, js.Peers(cfg.NodeID), opID, 3, 2*time.Second, nil)
	res := stageResult{Outcome: out.Result, Message: out.Message, Pin: key.Pin(), Standalone: standalone, Attempts: out.Attempts}
	switch out.Result {
	case service.JoinAccepted:
		js.Phase, js.Answer = joinstate.PhaseAccepted, out.Answer.Members
		res.ClusterID = out.Answer.ClusterID
		for _, m := range out.Answer.Members {
			res.Members = append(res.Members, m.Name)
		}
	case service.JoinUnknown:
		js.Phase = joinstate.PhaseUnknown
	default:
		// Join step 1 is known not to be applied: every handshake
		// failed and nothing was sent, or the leader refused.
		if err := joinstate.PutBack(dataDir, standalone, oneNodeState); err != nil {
			js.Phase = joinstate.PhaseUnknown
			_ = joinstate.Write(dataDir, js)
			return fail(fmt.Sprintf("the join failed before step 1 (%s), but putting the one-node state back failed: %v; it is in %s", out.Message, err, standalone))
		}
		if err := joinstate.Remove(dataDir); err != nil {
			return fail("the one-node state is back, but join.json could not be removed: " + err.Error())
		}
		res.Standalone = ""
		return res
	}
	if err := joinstate.Write(dataDir, js); err != nil {
		res.Message += "; recording the outcome failed: " + err.Error()
	}
	return res
}
