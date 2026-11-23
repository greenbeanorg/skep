package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/greenbeanorg/skep/internal/setup"
)

func runRemove(ctx context.Context, args []string, e env) error {
	e.setUmask()
	flags := flag.NewFlagSet("skep-setup remove", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var o setup.RemoveOptions
	flags.StringVar(&o.Root, "root", "/", "directory standing in for / (for tests)")
	planOnly := flags.Bool("plan", false, "only show what remove would do and keep (read-only)")
	flags.BoolVar(&o.KeepGuests, "keep-guests", false, "remove although Skep guests exist; nothing is stopped, nothing will manage them")
	flags.BoolVar(&o.RevertHost, "revert-host", false, "remove the lines setup added to rc.conf and loader.conf, where unchanged since")
	flags.BoolVar(&o.PurgeData, "purge-data", false, "delete /var/db/skep (node identity, databases, audit logs), agent.json, api.json and the skep account; always asks")
	yes := flags.Bool("yes", false, "do not ask (except for -purge-data)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !*planOnly {
		if err := requireRoot(o.Root, e); err != nil {
			return err
		}
	}
	if err := e.checkDataDir(o.Root); err != nil {
		return err
	}
	f, err := e.prober.Probe(ctx)
	if err != nil {
		return err
	}
	p, err := setup.ComputeRemove(ctx, o, f, e.runner, e.agentFor(o.Root))
	if err != nil {
		return err
	}
	mode := map[bool]string{true: " (read-only; nothing was changed)", false: ""}[*planOnly]
	fmt.Fprintf(e.stdout, "skep-setup remove, root %s%s\n", o.Root, mode)
	printItems(e.stdout, p.Items, true)
	fmt.Fprintln(e.stdout, " ", summary(p.Items))
	if p.Refused() {
		return &exitError{code: exitRefused, msg: "refused: nothing was changed"}
	}
	if *planOnly {
		return nil
	}
	if err := refuseFixtureServices(o.Root, e, p); err != nil {
		return err
	}
	if o.PurgeData && !e.confirm("This deletes the node identity, both databases and every audit log, and cannot be undone. Delete them?") {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	if !*yes && !e.confirm(fmt.Sprintf("Make these %d changes?", len(p.Actions()))) {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	a := e.applier(o.Root)
	a.Command = "remove"
	j, err := a.Apply(ctx, p)
	if err != nil {
		return runFailure(err)
	}
	fmt.Fprintf(e.stdout, "done: journal %s. Now remove the files: pkg delete skep\n", setup.JournalDir(o.Root, j.ID))
	return nil
}
