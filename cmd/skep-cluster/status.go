package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

func runStatus(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster status", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	socket := flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the running service")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	client := localapi.NewClient(*socket)
	st, err := client.Status(ctx)
	if err != nil {
		return err
	}
	h, err := client.Health(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(struct {
			Status localapi.Status `json:"status"`
			Health models.Health   `json:"health"`
		}{st, h}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, string(b))
		return nil
	}
	if r := st.Removed; r != nil {
		fmt.Fprintf(e.stdout, "Node ID:       %s\n", termSafe(st.NodeID))
		fmt.Fprintf(e.stdout, "Node name:     %s\n", termSafe(st.NodeName))
		fmt.Fprintf(e.stdout, "Node key pin:  %s\n", termSafe(st.Pin))
		fmt.Fprintf(e.stdout, "REMOVED:       from cluster %s, %s (%s)\n", termSafe(r.ClusterID), termSafe(r.How), termSafe(r.At))
		fmt.Fprintf(e.stdout, "Raft:          shut; listener: none\n")
		fmt.Fprintf(e.stdout, "Build:         %s\n", termSafe(st.Build))
		if st.Join != nil {
			fmt.Fprintf(e.stdout, "This node was joining: run \"skep-cluster join --abandon\" as root, which puts its one-node cluster back (after removing this node on a member, if that was not done).\n")
		} else {
			fmt.Fprintf(e.stdout, "Run \"skep-cluster leave --wipe\" as root (after removing this node on a member, if that was not done).\n")
		}
		printHealth(e, h)
		return nil
	}
	fmt.Fprintf(e.stdout, "Node ID:       %s\n", termSafe(st.NodeID))
	fmt.Fprintf(e.stdout, "Node name:     %s\n", termSafe(st.NodeName))
	fmt.Fprintf(e.stdout, "Cluster ID:    %s\n", termSafe(st.ClusterID))
	fmt.Fprintf(e.stdout, "Node key pin:  %s\n", termSafe(st.Pin))
	fmt.Fprintf(e.stdout, "Certificate:   valid until %s\n", st.CertificateNotAfter.Format(time.RFC3339))
	fmt.Fprintf(e.stdout, "Raft:          %s, term %d, commit %d, last %d\n", termSafe(st.Raft.State), st.Raft.Term, st.Raft.CommitIndex, st.Raft.LastIndex)
	switch sm := st.StateMachine; {
	case sm.Stalled != nil:
		fmt.Fprintf(e.stdout, "State machine: STOPPED at log index %d (applied up to %d): %s\n", sm.Stalled.Index, sm.AppliedIndex, termSafe(sm.Stalled.Reason))
	case sm.UpToDate && sm.AppliedIndex < st.Raft.AppliedIndex:
		// "applied 7 of 10" with only the library's own entries in
		// between is no lag (8a fix round finding).
		fmt.Fprintf(e.stdout, "State machine: up to date, last Skep entry %d (entries %d-%d are Raft's own)\n", sm.AppliedIndex, sm.AppliedIndex+1, st.Raft.AppliedIndex)
	case sm.UpToDate:
		fmt.Fprintf(e.stdout, "State machine: up to date, last Skep entry %d\n", sm.AppliedIndex)
	default:
		fmt.Fprintf(e.stdout, "State machine: applied %d, catching up (Raft applied %d, commit %d)\n", sm.AppliedIndex, st.Raft.AppliedIndex, st.Raft.CommitIndex)
	}
	fmt.Fprintf(e.stdout, "Keys accepted: by the %s\n", termSafe(st.AcceptedBy))
	if j := st.Join; j != nil {
		fmt.Fprintf(e.stdout, "Joining:       cluster %s, %s", termSafe(j.ClusterID), termSafe(j.Phase))
		if j.Message != "" {
			fmt.Fprintf(e.stdout, " (%s)", termSafe(j.Message))
		}
		fmt.Fprintln(e.stdout)
	}
	switch {
	case st.ListenAddress == "":
		fmt.Fprintf(e.stdout, "Listener:      none (single host, no network port)\n")
	case st.Listening:
		fmt.Fprintf(e.stdout, "Listener:      %s (open)\n", termSafe(st.ListenAddress))
	default:
		fmt.Fprintf(e.stdout, "Listener:      %s (NOT open)\n", termSafe(st.ListenAddress))
	}
	fmt.Fprintf(e.stdout, "Build:         %s\n", termSafe(st.Build))
	fmt.Fprintf(e.stdout, "Members (%d):\n", len(st.Members))
	for _, m := range st.Members {
		addr := m.Address
		if addr == "" {
			addr = "-"
		}
		role := "not a Raft server"
		for _, sv := range st.Servers {
			if sv.ID == m.ID {
				role = "non-voter"
				if sv.Voter {
					role = "voter"
				}
			}
		}
		fmt.Fprintf(e.stdout, "  %s  %s  %s, %s  %s  %s\n", termSafe(m.Name), termSafe(m.ID), termSafe(m.State), role, termSafe(addr), termSafe(m.Pin))
	}
	for _, r := range st.Removals {
		fmt.Fprintf(e.stdout, "Removal:       %s\n", termSafe(r))
	}
	if len(st.UnappliedRemoval) > 0 {
		fmt.Fprintf(e.stdout, "Not yet applied the last removal: %s\n", termSafe(strings.Join(st.UnappliedRemoval, ", ")))
	}
	if len(st.Pendings) > 0 {
		fmt.Fprintf(e.stdout, "Pending admissions (%d):\n", len(st.Pendings))
		for _, p := range st.Pendings {
			fmt.Fprintf(e.stdout, "  %s  %s  %s  %s  until %s\n", termSafe(p.Name), termSafe(p.NodeID), termSafe(p.Address), termSafe(p.Pin), time.Unix(p.ExpiresAt, 0).UTC().Format(time.RFC3339))
		}
	}
	printHealth(e, h)
	return nil
}

func printHealth(e env, h models.Health) {
	fmt.Fprintf(e.stdout, "Health:        %s\n", termSafe(string(h.Status)))
	names := make([]string, 0, len(h.Checks))
	for n := range h.Checks {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if c := h.Checks[n]; c.Status != models.HealthOK {
			fmt.Fprintf(e.stdout, "  %s: %s: %s\n", termSafe(n), termSafe(string(c.Status)), termSafe(c.Error))
		}
	}
}

func runHealth(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster health", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	socket := flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the running service")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	h, err := localapi.NewClient(*socket).Health(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(h, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, string(b))
	} else {
		printHealth(e, h)
	}
	if h.Status != models.HealthOK {
		return &exitCodeError{code: 1}
	}
	return nil
}

// termSafe replaces control characters (newlines too) and Unicode format
// characters (category Cf: the direction overrides and isolates, zero-width
// characters) with "?": for every string from another node or the stage
// that a command prints.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// termSafeLines is termSafe per line, keeping the line breaks.
func termSafeLines(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = termSafe(lines[i])
	}
	return strings.Join(lines, "\n")
}
