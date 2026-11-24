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
