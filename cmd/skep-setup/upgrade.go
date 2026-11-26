package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/pkg/version"
)

func runUpgrade(ctx context.Context, args []string, e env) error {
	e.setUmask()
	flags := flag.NewFlagSet("skep-setup upgrade", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	root := flags.String("root", "/", "directory standing in for / (for tests)")
	force := flags.Bool("force", false, "upgrade although tasks are pending or running: they are interrupted and must be resubmitted")
	wait := flags.Duration("wait", setup.DefaultTaskWait, "after stopping skep_api, wait this long for pending and running tasks to finish (0: do not wait)")
	keep := flags.Int("keep", dbbackup.DefaultKeep, "database backups to keep")
	yes := flags.Bool("yes", false, "do not ask")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *wait < 0 {
		return fmt.Errorf("-wait %s: must not be negative", *wait)
	}
	if *keep < 1 {
		return fmt.Errorf("-keep %d: at least one backup must be kept", *keep)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if err := requireRoot(*root, e); err != nil {
		return err
	}
	if *root != "/" && e.realRunner() {
		return &exitError{code: exitUsage, msg: "-root " + *root + ": upgrade stops and starts this host's services; it runs only on / (tests use a fake runner)"}
	}
	if err := e.checkDataDir(*root); err != nil {
		return err
	}
	if !*yes && !e.confirm("Stop skep_api, wait for unfinished tasks, back up both databases, restart skep_agent, initialise and enable skep_cluster where it is not, restart skep_cluster and skep_api, and import this host's users where it is alone? Running guests are not touched.") {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	agent := e.agentFor(*root)
	taskWait := *wait
	if taskWait == 0 {
		taskWait = -1 // Upgrader: 0 is the default, negative is no wait
	}
	u := &setup.Upgrader{Root: *root, Runner: e.runner, Prober: e.prober, Agent: agent, Now: e.now, Out: e.stdout,
		Self: version.Get(), Force: *force, Wait: taskWait, Keep: *keep, Sleep: e.sleep, Inode: e.inode,
		Cluster: e.cluster, Importer: e.importer}
	return upgradeExit(u.Upgrade(ctx))
}

// upgradeExit maps upgrade's outcome to an exit: 4 (incomplete) when
// changes were made and the result could not be verified, 3 for a refusal
// and for every other failure.
func upgradeExit(err error) error {
	if err == nil {
		return nil
	}
	var inc *setup.UpgradeIncomplete
	if errors.As(err, &inc) {
		return &exitError{code: exitIncomplete, msg: err.Error()}
	}
	return &exitError{code: exitRefused, msg: err.Error()}
}
