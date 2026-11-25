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
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

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
	if err := layout.CheckOldLayout(dataDir); err != nil {
		return cfg, key, 0, false, err
	}
	servers, err := store.ReadServers(layout.State(dataDir), cfg.NodeID)
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

// stageJoin runs service.StageJoin and reports its outcome.
func stageJoin(ctx context.Context, e env, configPath, dataDir, line string) stageResult {
	adm, err := joincode.ParseAdmission(line)
	if err != nil {
		return fail(err.Error())
	}
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	key, err := nodekey.Load(dataDir)
	if err != nil {
		return fail(err.Error())
	}
	out := service.StageJoin(ctx, dataDir, cfg, key, adm, time.Now(), 2*time.Second)
	res := stageResult{Outcome: out.Outcome, Message: out.Message, Pin: key.Pin(), Standalone: out.Standalone, Attempts: out.Attempts}
	if out.Answer != nil {
		res.ClusterID = out.Answer.ClusterID
		for _, m := range out.Answer.Members {
			res.Members = append(res.Members, m.Name)
		}
	}
	return res
}
