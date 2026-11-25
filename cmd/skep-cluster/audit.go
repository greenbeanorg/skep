package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
)

// runAudit is "skep-cluster audit verify [-dir DIR]" (D4 of step 8b-2b):
// root only, local and read-only: it opens every file read-only, creates
// nothing and takes no lock, so it works with the service running or
// stopped. Exit codes as "skep-agent audit verify": 0 nothing wrong, 1 a
// break found or the audit could not be read, 2 a usage error.
func runAudit(_ context.Context, args []string, e env) error {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintf(e.stderr, "usage: skep-cluster audit verify [-dir DIR]\n")
		return &exitCodeError{code: 2}
	}
	flags := flag.NewFlagSet("skep-cluster audit verify", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	dataDir := flags.String("data-dir", clusterconf.DefaultDataDir, "data directory; the audit is its audit/ directory")
	dir := flags.String("dir", "", "verify the audit in this directory instead (for example a copy)")
	if err := flags.Parse(args[1:]); err != nil {
		return &exitCodeError{code: 2}
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(e.stderr, "unexpected argument %q\n", termSafe(flags.Arg(0)))
		return &exitCodeError{code: 2}
	}
	if e.euid != 0 {
		return errNeedsRoot("audit verify")
	}
	path := *dir
	if path == "" {
		path = audit.Dir(*dataDir)
	}
	rep, err := audit.Verify(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Cluster audit:   %s\n", termSafe(path))
	fmt.Fprintf(e.stdout, "Records:         %d in %d segment(s)\n", rep.Records, len(rep.Segments))
	if sealed := rep.Sealed(); len(sealed) > 0 {
		fmt.Fprintf(e.stdout, "Sealed segments: %s\n", termSafe(strings.Join(sealed, ", ")))
	} else {
		fmt.Fprintf(e.stdout, "Sealed segments: none (a segment is sealed when the next one exists)\n")
	}
	if len(rep.Segments) > 0 {
		fmt.Fprintf(e.stdout, "Open segment:    %s\n", termSafe(rep.Segments[len(rep.Segments)-1]))
	}
	if rep.First != nil {
		fmt.Fprintf(e.stdout, "First record:    %s\n", describeRecord(*rep.First))
		fmt.Fprintf(e.stdout, "Last record:     %s\n", describeRecord(*rep.Last))
		fmt.Fprintf(e.stdout, "Highest entry:   epoch %d, index %d\n", rep.HighEpoch, rep.HighIndex)
	}
	if b := rep.Break; b != nil {
		fmt.Fprintf(e.stdout, "BROKEN:          %s line %d: %s\n", termSafe(b.File), b.Line, termSafe(b.Reason))
		return &exitCodeError{code: 1, msg: "the cluster audit is broken"}
	}
	if rep.Records == 0 {
		fmt.Fprintf(e.stdout, "The audit is empty.\n")
		return nil
	}
	fmt.Fprintf(e.stdout, "Every hash and every link checks out.\n")
	return nil
}

func describeRecord(r audit.Record) string {
	s := fmt.Sprintf("seq %d, %s, %s", r.Seq, r.LocalTime, r.Kind)
	if r.Index != 0 || r.Epoch != 0 {
		s += fmt.Sprintf(" (epoch %d, index %d)", r.Epoch, r.Index)
	}
	if r.EntryType != "" {
		s += " " + r.EntryType
		if r.Outcome != "" {
			s += " " + r.Outcome
		}
	}
	return termSafe(s)
}
