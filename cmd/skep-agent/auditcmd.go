package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/auditcmd"
	"github.com/greenbeanorg/skep/internal/config"
)

// runAudit runs "skep-agent audit verify" and "skep-agent audit export":
// local commands on the agent's own audit archive. They open agent.db
// directly, so they need to run as root, and they work whether or not the
// agent is running, except "export --unsealed" (below). They open the
// database read-write but write no entries, create no database and migrate
// nothing; see internal/auditcmd for what SQLite itself may do to the
// files. "export --unsealed" also covers entries not yet sealed: root only,
// read-only, never through the API or the socket.
func runAudit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	usage := auditcmd.Usage("skep-agent") + `

  skep-agent audit export --unsealed [flags]
        as root, while the agent runs: print the entries not yet sealed, in
        the same format, opening the database read-only. It refuses while
        the agent is stopped: SQLite would create the -wal and -shm files
        even for a read-only open and leave them behind`
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return errors.New("missing audit command")
	}
	command := args[0]
	if command != "verify" && command != "export" {
		fmt.Fprintln(stderr, usage)
		return fmt.Errorf("unknown audit command %q", command)
	}

	flags := flag.NewFlagSet("skep-agent audit "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		configPath   = flags.String("config", "", "path to the JSON configuration file (default "+config.DefaultConfigPath+" if it exists)")
		dataDir      = flags.String("data-dir", "", "state directory")
		dev          = flags.Bool("dev", false, "development mode: use the local data directory")
		allowMissing *bool
		month        *string
		unsealed     = new(bool)
	)
	if command == "verify" {
		allowMissing = flags.Bool("allow-missing", false, "accept sealed segments whose file is not in the archive directory")
	} else {
		month = flags.String("month", "", "export one month, as YYYY-MM (default: every sealed segment)")
		unsealed = flags.Bool("unsealed", false, "export the entries not yet sealed instead, read-only (root, while the agent runs)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *unsealed {
		if *month != "" {
			return errors.New("--unsealed and --month cannot be combined")
		}
		if euid := os.Geteuid(); euid != 0 {
			return fmt.Errorf("audit export --unsealed must run as root (running as uid %d)", euid)
		}
	}
	overrides := config.Overrides{DataDir: *dataDir}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dev" {
			overrides.DevMode = dev
		}
	})
	cfg, err := config.Load(config.LoadOptions{Path: *configPath, Overrides: overrides})
	if err != nil {
		return err
	}

	open := auditcmd.Open
	if *unsealed {
		open = auditcmd.OpenReadOnly
	}
	db, err := open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	archive, err := auditarchive.New(auditarchive.Options{
		DB: db, Log: auditarchive.AgentLog, Dir: cfg.AuditArchiveDir(), Logger: quietLogger(stderr),
	})
	if err != nil {
		return err
	}
	if command == "verify" {
		return auditcmd.Verify(ctx, archive, *allowMissing, stdout)
	}
	if *unsealed {
		n, err := auditcmd.ExportUnsealed(ctx, archive, stdout)
		if err == nil {
			fmt.Fprintf(stderr, "%d entries exported; they are not sealed yet, so the archive's hash verification does not cover them\n", n)
		}
		return err
	}
	return auditcmd.Export(ctx, archive, *month, stdout)
}
