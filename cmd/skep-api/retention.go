package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"database/sql"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/auditcmd"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Retention of the access audit log (ADR 0007, decision 7). skep-api keeps
// custody of its own log: it seals the monthly segments of api.db into its
// own archive directory and removes live entries once they are sealed,
// verified, and old enough. The agent never opens either.

const (
	// actionAuditArchive is the access audit action of an archive run.
	// skep-api has no task engine, so the run is recorded as an entry.
	actionAuditArchive = "audit.archive"
	// archiveInterval is how often the archive run happens.
	archiveInterval = 24 * time.Hour
	// archiveRetry is how soon a failed run is tried again.
	archiveRetry = time.Hour
	// archiveTimeout bounds one run.
	archiveTimeout = 30 * time.Minute
	// apiSyslogTag is the tag of skep-api's own syslog lines.
	apiSyslogTag = "skep-api"
)

// nodeSource is where skep-api learns the node's ID: the agent.
type nodeSource interface {
	System(ctx context.Context) (models.System, error)
}

// newAccessArchive returns the archive of the access audit log, or nil
// when archiving is turned off: then nothing is sealed and no entry is
// ever removed.
func newAccessArchive(db *sql.DB, cfg config.API, agent nodeSource, log *slog.Logger, now func() time.Time) (*auditarchive.Archiver, error) {
	if !cfg.Retention.ArchiveEnabled() {
		return nil, nil
	}
	var archive *auditarchive.Archiver
	opts := auditarchive.Options{
		DB:     db,
		Log:    auditarchive.AccessLog,
		Dir:    cfg.AuditArchiveDir(),
		Logger: log,
		Now:    now,
		// Segment names and headers carry the node ID, which only the
		// agent knows. When the agent cannot be asked, the ID recorded
		// with the previous segment is the same one.
		NodeID: func(ctx context.Context) (string, error) {
			// skep-api asks as itself: no user is involved.
			sys, err := agent.System(agentproto.WithCaller(ctx, agentproto.Caller{
				ActorID: agentproto.APIActor, ActorName: agentproto.APIActor,
			}))
			if err == nil && sys.NodeID != "" {
				return sys.NodeID, nil
			}
			segs, serr := archive.Segments(ctx)
			if serr == nil && len(segs) > 0 {
				return segs[len(segs)-1].NodeID, nil
			}
			return "", fmt.Errorf("the agent did not answer and no earlier segment names the node: %w", errors.Join(err, serr))
		},
	}
	if !cfg.DevMode {
		opts.Notify = auditarchive.SyslogNotifier(apiSyslogTag, auditarchive.AccessLog.Name, log)
	}
	var err error
	archive, err = auditarchive.New(opts)
	return archive, err
}

// archiveLoop runs the archive once now and then every interval, sooner
// after a failure, until ctx is done.
func archiveLoop(ctx context.Context, archive *auditarchive.Archiver, liveDays int, access *apidb.AccessLog,
	log *slog.Logger, interval, retry time.Duration) {
	for {
		wait := interval
		if err := archiveOnce(ctx, archive, liveDays, access, log); err != nil {
			wait = retry
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// archiveOnce performs one archive run and records it in the access audit
// log: what was sealed, what was removed, and how it ended.
func archiveOnce(ctx context.Context, archive *auditarchive.Archiver, liveDays int, access *apidb.AccessLog, log *slog.Logger) error {
	runCtx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	sum, err := archive.Cycle(runCtx, liveDays)
	if ctx.Err() != nil {
		return ctx.Err() // shutting down: the next start runs again
	}

	details := map[string]any{
		"live_days":         liveDays,
		"segments_verified": sum.Verified,
		"entries_removed":   sum.Removed.Entries,
	}
	if len(sum.Removed.Months) > 0 {
		details["months_removed"] = sum.Removed.Months
	}
	if len(sum.Missing) > 0 {
		details["segments_missing"] = sum.Missing
	}
	if len(sum.Sealed) > 0 {
		sealed := make([]map[string]any, 0, len(sum.Sealed))
		for _, s := range sum.Sealed {
			sealed = append(sealed, map[string]any{
				"month": s.Month, "first_id": s.FirstID, "last_id": s.LastID, "entries": s.Entries, "sha256": s.Hash,
			})
		}
		details["segments_sealed"] = sealed
	}
	outcome := audit.Success
	if err != nil {
		outcome = audit.Failure
		switch {
		case errors.Is(err, auditarchive.ErrInvalid):
			details["error_code"] = "audit_archive_invalid"
		case errors.Is(err, auditarchive.ErrMissing):
			details["error_code"] = "audit_archive_missing"
		default:
			details["error_code"] = "audit_archive_failed"
		}
		// The cause names files; it goes to the log, not into the entry
		// that the API serves.
		log.Error("access audit archive run failed; no audit entry was lost", "error", err)
	} else {
		log.Info("access audit archive run finished", "segments_verified", sum.Verified,
			"segments_sealed", len(sum.Sealed), "entries_removed", sum.Removed.Entries)
	}
	writeAudit(ctx, access, log, audit.AccessEntry{
		ActorName: audit.System, Action: actionAuditArchive, ResourceType: "audit",
		Outcome: outcome, Details: details,
	})
	return err
}

// runAudit runs "skep-api audit verify" and "skep-api audit export": the
// same local commands as skep-agent's, on the access audit log. Like the
// other local commands they give up root for the service account first.
func runAudit(ctx context.Context, args []string, e env) error {
	usage := auditcmd.Usage("skep-api")
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, usage)
		return errors.New("missing audit command")
	}
	command := args[0]
	if command != "verify" && command != "export" {
		fmt.Fprintln(e.stderr, usage)
		return fmt.Errorf("unknown audit command %q", command)
	}
	flags := newLocalFlags("skep-api audit "+command, e.stderr)
	var (
		allowMissing *bool
		month        *string
	)
	if command == "verify" {
		allowMissing = flags.set.Bool("allow-missing", false, "accept sealed segments whose file is not in the archive directory")
	} else {
		month = flags.set.String("month", "", "export one month, as YYYY-MM (default: every sealed segment)")
	}
	cfg, err := flags.prepare(args[1:], e)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(e.stderr, usage)
		}
		return err
	}
	db, err := auditcmd.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	archive, err := auditarchive.New(auditarchive.Options{
		DB: db, Log: auditarchive.AccessLog, Dir: cfg.AuditArchiveDir(), Logger: quietLogger(e.stderr),
	})
	if err != nil {
		return err
	}
	if command == "verify" {
		return auditcmd.Verify(ctx, archive, *allowMissing, e.stdout)
	}
	return auditcmd.Export(ctx, archive, *month, e.stdout)
}
