package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

// agentStartFS is what check runs the agent's start check on. The check
// needs root (agent.json is 0600) and the real root.
func (e env) agentStartFS(root string) (startcheck.FS, string) {
	if e.startFS != nil {
		return e.startFS, ""
	}
	if filepath.Clean(root) != "/" {
		return nil, "-root is set"
	}
	if e.euid != 0 {
		return nil, "run skep-setup check as root"
	}
	fsys, ok := hostStartFS()
	if !ok {
		return nil, "it runs on FreeBSD only"
	}
	return fsys, ""
}

// reportAgentWithoutPlan prints the agent's state and start check when
// check has no plan because -mgmt is missing: neither needs
// them. The caller still returns the usage error, so the exit code is
// unchanged.
func reportAgentWithoutPlan(ctx context.Context, root string, e env) {
	fmt.Fprintf(e.stdout, "skep-setup check, root %s (read-only; nothing was changed): no plan without -mgmt\n", root)
	var f setup.Facts
	if fsys, _ := e.agentStartFS(root); fsys != nil {
		var err error
		if f, err = e.prober.Probe(ctx); err != nil {
			fmt.Fprintf(e.stdout, "  agent: state and start check not read (%v)\n", err)
			return
		}
	}
	printItems(e.stdout, reportAgent(root, f, e), false)
}

// reportAgent prints whether the agent runs and its start check, and
// returns the failed check item for a refusal or a check that could not run.
func reportAgent(root string, f setup.Facts, e env) []setup.Item {
	fsys, why := e.agentStartFS(root)
	if fsys == nil {
		fmt.Fprintf(e.stdout, "  agent: state and start check not read (%s)\n", why)
		return nil
	}
	state := "not running"
	if f.Running["skep_agent"] {
		state = "running"
	}
	fmt.Fprintf(e.stdout, "  agent: %s\n", state)
	r := setup.CheckAgentStart(root, fsys)
	if len(r.Paths) > 0 {
		fmt.Fprintf(e.stdout, "  agent start check paths (each with its parents up to /): %s\n", strings.Join(r.Paths, ", "))
	}
	if r.ArgsIgnored {
		fmt.Fprintln(e.stdout, "  agent start check: skep_agent_args is set and is not read; it could change these paths")
	}
	switch {
	case r.Refusal != nil:
		it := setup.Item{Kind: setup.Manual, Area: "agent start check", What: r.Refusal.Path + " " + r.Refusal.Why}
		if r.Refusal.Fix != "" {
			it.Detail = "Fix: " + r.Refusal.Fix
		}
		fmt.Fprintln(e.stdout, "  agent start check: REFUSED; the agent will not start")
		return []setup.Item{it}
	case r.Problem != "":
		fmt.Fprintln(e.stdout, "  agent start check: not confirmed; it could not run")
		return []setup.Item{{Kind: setup.Manual, Area: "agent start check", What: "not confirmed: " + r.Problem}}
	}
	fmt.Fprintln(e.stdout, "  agent start check: passed")
	return nil
}

// reportCluster prints, as root on the real root, whether skep_cluster
// runs, one line of its status, and the import state of the host's users
// (D9 of step 8c-2). Every string from skep-cluster or skep-api is
// cleaned. It returns the items the operator has to act on.
func reportCluster(ctx context.Context, root string, f setup.Facts, e env) []setup.Item {
	cl, why := e.clusterFor(root)
	if cl == nil {
		fmt.Fprintf(e.stdout, "  cluster: state not read (%s)\n", why)
		return nil
	}
	if f.Running["skep_cluster"] {
		fmt.Fprintln(e.stdout, "  cluster: skep_cluster running")
		if v, err := cl.Status(ctx); err != nil {
			fmt.Fprintf(e.stdout, "  cluster: status not read (%s)\n", setup.CleanLines(err.Error()))
		} else {
			fmt.Fprintf(e.stdout, "  cluster: %s\n", v.Summary())
		}
	} else {
		fmt.Fprintln(e.stdout, "  cluster: skep_cluster not running; skep-api then has no quorum and lets only administrators log in, degraded")
	}
	im, why := e.importerFor(root)
	if im == nil {
		fmt.Fprintf(e.stdout, "  users: import state not read (%s)\n", why)
		return nil
	}
	st, err := im.State(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(e.stdout, "  users: import state not read (%s)\n", setup.CleanLines(err.Error()))
	case st == setup.ImportPending:
		return []setup.Item{{Kind: setup.Manual, Area: "users", What: "import of this host's users: pending",
			Detail: "user changes answer 503 users_import_pending until it is done; alone, skep-setup upgrade does it; in a cluster of several nodes follow docs/installation/upgrading.md",
			Steps:  []string{setup.ImportCommand}}}
	case st == setup.ImportUnmigrated:
		return []setup.Item{{Kind: setup.Manual, Area: "users", What: "api.db is older than the installed skep-api",
			Steps: []string{"skep-setup upgrade"}}}
	default:
		fmt.Fprintf(e.stdout, "  users: import state %s\n", st)
	}
	return nil
}
