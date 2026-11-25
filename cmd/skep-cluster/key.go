package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
)

// refuseDuringRotation refuses a command while rotate.json exists (step
// 8b-2b, D1): join-request, join and listen -none.
func refuseDuringRotation(dataDir, cmd string) error {
	ok, err := layout.Exists(filepath.Join(dataDir, layout.RotateFile))
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("refused: this node is rotating its key (%s exists); \"skep-cluster %s\" waits until the rotation has ended (\"skep-cluster key rotate --abort\" aborts it); nothing was changed",
			filepath.Join(dataDir, layout.RotateFile), cmd)
	}
	return nil
}

// runKey is "skep-cluster key rotate [-wait D]" and "skep-cluster key
// rotate --abort" (D1 of step 8b-2b): root, local, through the socket. No
// API operation changes a pin; "--api" arrives with forwarding (step 8e).
// Exit codes: 0 the old pin is gone from this node's applied membership
// (or the abort finished), 3 the wait ended first, 1 a refusal.
func runKey(ctx context.Context, args []string, e env) error {
	if len(args) == 0 || args[0] != "rotate" {
		fmt.Fprintf(e.stderr, "usage: skep-cluster key rotate [-wait D] | skep-cluster key rotate --abort\n")
		return &exitCodeError{code: 2}
	}
	flags := flag.NewFlagSet("skep-cluster key rotate", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		socket = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		wait   = flags.Duration("wait", 2*time.Minute, "how long to wait for the rotation to end")
		abort  = flags.Bool("abort", false, "abort this node's rotation; only while it still uses its old key")
		api    = flags.Bool("api", false, "rotate skep-api's node-to-node key (not in this build)")
	)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", termSafe(flags.Arg(0)))
	}
	if *api {
		return errors.New("refused: skep-api's node-to-node key arrives with forwarding (step 8e); nothing was changed")
	}
	if e.euid != 0 {
		return errNeedsRoot("key rotate")
	}
	client := localapi.NewClient(*socket)
	if *abort {
		res, err := client.AbortRotation(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "Aborting the key rotation of this node: the next pin %s is dropped, the pin stays %s.\n", termSafe(res.NewPin), termSafe(res.OldPin))
		if res.Pending != "" {
			fmt.Fprintf(e.stdout, "%s\n", termSafe(res.Pending))
		}
		return waitRotation(ctx, e, client, res, *wait, true)
	}
	res, err := client.RotateKey(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Rotating this node's key:\n  old pin: %s\n  new pin: %s\n", termSafe(res.OldPin), termSafe(res.NewPin))
	if res.AddIndex != 0 {
		fmt.Fprintf(e.stdout, "The leader added the new pin (log index %d). Every member now accepts both keys as this node.\n", res.AddIndex)
	} else if res.Pending != "" {
		fmt.Fprintf(e.stdout, "%s\n", termSafe(res.Pending))
	}
	return waitRotation(ctx, e, client, res, *wait, false)
}

// waitRotation waits until this node's applied membership no longer has
// the pin being dropped (the old pin, or with abort the next pin), and
// says what it waits for when the wait ends first.
func waitRotation(ctx context.Context, e env, client *localapi.Client, res localapi.RotateResult, wait time.Duration, abort bool) error {
	deadline := time.Now().Add(wait)
	var last localapi.Status
	for {
		st, err := client.Status(ctx)
		if err == nil {
			last = st
			for _, m := range st.Members {
				if m.ID != st.NodeID {
					continue
				}
				switch {
				case !abort && m.Pin == res.NewPin && m.NextPin == "":
					fmt.Fprintf(e.stdout, "Done: this node's pin is %s on its applied membership; the old pin is no longer accepted.\n", termSafe(res.NewPin))
					return nil
				case abort && m.Pin == res.OldPin && m.NextPin == "" && st.Rotation == nil:
					fmt.Fprintf(e.stdout, "Aborted: this node's pin stays %s; the next key is deleted.\n", termSafe(res.OldPin))
					return nil
				}
			}
			if st.Rotation == nil && !abort {
				return errors.New("this node's rotation ended without the new pin (refused or aborted); see \"skep-cluster status\"")
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(e.stdout, "After %s the rotation has not ended.\n", wait)
			for _, r := range last.Rotations {
				fmt.Fprintf(e.stdout, "  %s\n", termSafe(r))
			}
			if len(last.Rotations) == 0 && last.Rotation != nil {
				fmt.Fprintf(e.stdout, "  the leader has not added the next pin yet; this node asks every 10 s\n")
			}
			fmt.Fprintf(e.stdout, "It ends by itself; \"skep-cluster health\" on any member says what it waits for.\n")
			if !abort {
				fmt.Fprintf(e.stdout, "A member that stays down can be removed (\"skep-cluster remove NAME\"), or the rotation aborted (\"skep-cluster key rotate --abort\") while this node still uses its old key.\n")
			}
			return &exitCodeError{code: 3}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
