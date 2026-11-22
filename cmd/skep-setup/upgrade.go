package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/pkg/version"
)

func runUpgrade(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-setup upgrade", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	root := flags.String("root", "/", "directory standing in for / (for tests)")
	force := flags.Bool("force", false, "upgrade while tasks are running (they become interrupted)")
	keep := flags.Int("keep", dbbackup.DefaultKeep, "database backups to keep")
	yes := flags.Bool("yes", false, "do not ask")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if err := requireRoot(*root, e); err != nil {
		return err
	}
	if !*yes && !e.confirm("Back up both databases and restart skep_api and skep_agent? Running guests are not touched.") {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	agent := e.agent
	if agent == nil {
		agent = setup.SocketAgent{Path: filepath.Join(*root, "var/run/skep/agent.sock")}
	}
	u := &setup.Upgrader{Root: *root, Runner: e.runner, Prober: e.prober, Agent: agent, Now: e.now, Out: e.stdout,
		Self: version.Get(), Force: *force, Keep: *keep, Sleep: e.sleep, Inode: e.inode}
	err := u.Upgrade(ctx)
	var r *setup.UpgradeRefusal
	if errors.As(err, &r) {
		return &exitError{code: exitRefused, msg: err.Error()}
	}
	if err != nil {
		return &exitError{code: exitRefused, msg: err.Error()}
	}
	return nil
}
