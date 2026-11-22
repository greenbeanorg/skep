package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/pkg/version"
)

// requireRoot refuses a command that changes the real host unless it runs
// as root. A fixture root (tests) needs no privilege.
func requireRoot(root string, e env) error {
	if root == "/" && e.euid != 0 {
		return &exitError{code: exitUsage, msg: "this command changes the host; run it as root"}
	}
	return nil
}

func (e env) applier(root string) *setup.Applier {
	return &setup.Applier{Root: root, Runner: e.runner, Now: e.now, Chown: e.chown, Out: e.stdout,
		SkepVersion: version.Get().String(), Sleep: e.sleep}
}

func runApply(ctx context.Context, args []string, e env) error {
	e.setUmask()
	var yes bool
	o, p, err := computePlan(ctx, "apply", args, e, func(fs *flag.FlagSet) {
		fs.BoolVar(&yes, "yes", false, "do not ask before making the changes")
	}, func(o setup.Options) error { return requireRoot(o.Root, e) })
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "skep-setup apply, root %s\n", o.Root)
	printItems(e.stdout, p.Items, false)
	fmt.Fprintln(e.stdout, " ", summary(p.Items))
	if p.Refused() {
		return &exitError{code: exitRefused, msg: "refused: nothing was changed"}
	}
	if err := refuseFixtureServices(o.Root, e, p); err != nil {
		return err
	}
	acts := p.Actions()
	if len(acts) == 0 {
		fmt.Fprintln(e.stdout, "nothing to do: the host is already as planned")
		return nil
	}
	if !yes && !e.confirm(fmt.Sprintf("Make these %d changes?", len(acts))) {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	j, err := e.applier(o.Root).Apply(ctx, p)
	var adminErr *setup.AdminCreateError
	if errors.As(err, &adminErr) {
		return &exitError{code: exitRefused, msg: fmt.Sprintf("%v. Create the administrator by running the same command again; it plans only what is left:\n  skep-setup apply %s",
			err, strings.Join(args, " "))}
	}
	if err != nil {
		if errors.Is(err, setup.ErrNothingToDo) {
			fmt.Fprintln(e.stdout, err)
			return nil
		}
		return &exitError{code: exitRefused, msg: err.Error()}
	}
	fmt.Fprintf(e.stdout, "done: %d changes, journal %s (undo with: skep-setup rollback %s)\n",
		len(j.Entries), setup.JournalDir(o.Root, j.ID), j.ID)
	for _, it := range p.Items {
		if it.Kind == setup.Add && it.Action != nil && it.Action.Name == "kern.racct.enable" {
			fmt.Fprintln(e.stdout, "RACCT takes effect only after a reboot; setup never reboots.")
		}
	}
	if n := count(p.Items, setup.Manual); n > 0 {
		fmt.Fprintf(e.stdout, "%d item(s) are left to you; see \"manual\" above.\n", n)
	}
	return nil
}

func runRollback(_ context.Context, args []string, e env) error {
	e.setUmask()
	flags := flag.NewFlagSet("skep-setup rollback", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	root := flags.String("root", "/", "directory standing in for / (for tests)")
	yes := flags.Bool("yes", false, "do not ask")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("usage: skep-setup rollback [-yes] [JOURNAL-ID]")
	}
	if err := requireRoot(*root, e); err != nil {
		return err
	}
	id := flags.Arg(0)
	target, err := setup.RollbackTarget(*root, id)
	if err != nil {
		return &exitError{code: exitRefused, msg: err.Error()}
	}
	fmt.Fprintln(e.stdout, "rollback", target.Describe())
	if !*yes && !e.confirm("Roll back run "+target.ID+"? Files changed since are left alone.") {
		return &exitError{code: exitPending, msg: "not confirmed: nothing was changed"}
	}
	r, err := setup.Rollback(*root, target.ID, e.now())
	for _, f := range r.Restored {
		fmt.Fprintln(e.stdout, "  restored", f)
	}
	for _, f := range r.Removed {
		fmt.Fprintln(e.stdout, "  removed ", f)
	}
	for _, f := range r.Kept {
		fmt.Fprintln(e.stdout, "  kept    ", f)
	}
	if err != nil {
		return &exitError{code: exitRefused, msg: err.Error()}
	}
	fmt.Fprintf(e.stdout, "rolled back %s\n", r.ID)
	return nil
}
