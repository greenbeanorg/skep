// Package retention is the agent's maintenance.retention task
// (docs/decisions/0007-retention.md, decisions 1 to 3): once a day it
// verifies the audit archive, seals the audit segments that are due,
// removes the live audit entries the configuration allows, and deletes
// finished tasks that are older than the retention age.
//
// It runs as a task, with the system as its actor, so that the removal of
// history is itself on the record: the task's "finished" audit entry says
// what was removed.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/tasks"
)

const (
	// TaskType is the type of the retention task.
	TaskType = "maintenance.retention"
	// ResourceType and ResourceID name the task's resource. A resource has
	// at most one unfinished task, so two runs never overlap, and the
	// newest finished task of a resource is never pruned, so the last run
	// is always on record.
	ResourceType = "maintenance"
	ResourceID   = "retention"

	// CodeArchiveInvalid is the error code of a run that found the audit
	// archive altered or its chain broken. Nothing was sealed or removed.
	CodeArchiveInvalid = "audit_archive_invalid"
	// CodeArchiveMissing is the error code of a run that needed a segment
	// file that is not in the archive directory. Its entries were kept.
	CodeArchiveMissing = "audit_archive_missing"
	// CodeArchiveFailed is the error code of a run whose audit work
	// failed for another reason, for example a full disk.
	CodeArchiveFailed = "audit_archive_failed"

	// Interval is how often the task runs.
	Interval = 24 * time.Hour
	// Timeout is the task's time limit.
	Timeout = 30 * time.Minute
	// Batch is the number of tasks deleted per transaction, so that no
	// transaction holds the write lock for long.
	Batch = 500
)

// TaskPruner deletes finished tasks. The SQLite implementation is
// database.TaskStore.
type TaskPruner interface {
	// PruneFinished deletes up to limit finished tasks that finished
	// before cutoff, with their events, and returns how many of each.
	PruneFinished(ctx context.Context, cutoff time.Time, limit int) (tasks, events int64, err error)
}

// Options configures a Runner.
type Options struct {
	// Tasks deletes old tasks.
	Tasks TaskPruner
	// TasksDays is the retention age of finished tasks. 0 keeps them
	// forever.
	TasksDays int
	// Archive is the agent's audit archive. Nil when archiving is off:
	// then nothing is sealed and no audit entry is ever removed.
	Archive *auditarchive.Archiver
	// AuditLiveDays is how long sealed entries stay in the database after
	// their month ended. 0 never removes them.
	AuditLiveDays int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

// Runner performs retention runs.
type Runner struct {
	opts Options
}

// New returns a Runner.
func New(opts Options) (*Runner, error) {
	if opts.Tasks == nil {
		return nil, errors.New("retention: a task pruner is required")
	}
	if opts.TasksDays < 0 || opts.AuditLiveDays < 0 {
		return nil, errors.New("retention: ages must not be negative")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runner{opts: opts}, nil
}

// Result is the result of a retention task.
type Result struct {
	Tasks TaskResult `json:"tasks"`
	// Audit is absent when archiving is off.
	Audit *auditarchive.Summary `json:"audit,omitempty"`
}

// TaskResult says which tasks were deleted.
type TaskResult struct {
	// RetentionDays is the configured age; 0 means tasks are kept forever.
	RetentionDays int `json:"retention_days"`
	// Cutoff is the instant before which finished tasks were deleted.
	Cutoff        *time.Time `json:"cutoff,omitempty"`
	TasksRemoved  int64      `json:"tasks_removed"`
	EventsRemoved int64      `json:"events_removed"`
}

// AuditDetails implements tasks.Audited: what was removed, and which
// segments were sealed, goes into the task's "finished" audit entry.
func (r Result) AuditDetails() map[string]any {
	d := map[string]any{
		"tasks_retention_days": r.Tasks.RetentionDays,
		"tasks_removed":        r.Tasks.TasksRemoved,
		"events_removed":       r.Tasks.EventsRemoved,
	}
	if r.Tasks.Cutoff != nil {
		d["tasks_cutoff"] = r.Tasks.Cutoff.UTC().Format(time.RFC3339)
	}
	if r.Audit == nil {
		d["audit_archive"] = "off"
		return d
	}
	d["audit_segments_verified"] = r.Audit.Verified
	d["audit_entries_removed"] = r.Audit.Removed.Entries
	if len(r.Audit.Removed.Months) > 0 {
		d["audit_months_removed"] = r.Audit.Removed.Months
	}
	if len(r.Audit.Missing) > 0 {
		d["audit_segments_missing"] = r.Audit.Missing
	}
	if len(r.Audit.Sealed) > 0 {
		sealed := make([]map[string]any, 0, len(r.Audit.Sealed))
		for _, s := range r.Audit.Sealed {
			sealed = append(sealed, map[string]any{
				"month": s.Month, "first_id": s.FirstID, "last_id": s.LastID,
				"entries": s.Entries, "sha256": s.Hash,
			})
		}
		d["audit_segments_sealed"] = sealed
	}
	return d
}

// Definition returns the maintenance.retention task type. It is not
// cancellable: a run is short, and stops by itself between batches when
// the agent shuts down.
func (r *Runner) Definition() tasks.Definition {
	return tasks.Definition{
		Type:    TaskType,
		Timeout: Timeout,
		Run: func(ctx context.Context, _ tasks.Task, report tasks.Reporter) (any, error) {
			return r.Run(ctx, report)
		},
	}
}

// Run performs one retention run. If it fails nothing is lost: the next
// run starts from the same rules.
func (r *Runner) Run(ctx context.Context, report tasks.Reporter) (Result, error) {
	var res Result
	res.Tasks.RetentionDays = r.opts.TasksDays

	// The audit archive first: it is verified at the start of every run.
	var auditErr error
	if r.opts.Archive != nil {
		report.Progress("verifying and sealing the audit archive", nil)
		sum, err := r.opts.Archive.Cycle(ctx, r.opts.AuditLiveDays)
		res.Audit, auditErr = &sum, err
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}

	// Tasks are independent of the archive and are pruned even when the
	// audit work failed.
	if r.opts.TasksDays > 0 {
		// Whole seconds: the cutoff is written into the audit log.
		cutoff := r.opts.Now().UTC().Truncate(time.Second).AddDate(0, 0, -r.opts.TasksDays)
		res.Tasks.Cutoff = &cutoff
		report.Progress("deleting finished tasks older than the retention age",
			map[string]any{"cutoff": cutoff.Format(time.RFC3339), "retention_days": r.opts.TasksDays})
		for {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			n, events, err := r.opts.Tasks.PruneFinished(ctx, cutoff, Batch)
			if err != nil {
				return res, err
			}
			res.Tasks.TasksRemoved += n
			res.Tasks.EventsRemoved += events
			if n == 0 {
				break
			}
		}
	}
	r.opts.Logger.Info("retention run finished",
		"tasks_removed", res.Tasks.TasksRemoved, "events_removed", res.Tasks.EventsRemoved,
		"audit_archive_error", auditErr)

	if auditErr != nil {
		// The cause names files and goes to the log; the task carries a
		// code and a message for an administrator. What was removed is
		// recorded even though the task failed.
		r.opts.Logger.Error("retention: audit archive run failed", "error", auditErr)
		f := &tasks.Failure{Details: res.AuditDetails()}
		switch {
		case errors.Is(auditErr, auditarchive.ErrInvalid):
			f.Code = CodeArchiveInvalid
			f.Message = "the audit archive failed verification; nothing was sealed and no audit entry was removed. " +
				"Run 'skep-agent audit verify' on the host"
		case errors.Is(auditErr, auditarchive.ErrMissing):
			f.Code = CodeArchiveMissing
			f.Message = "an audit archive segment that is due for removal is not in the archive directory; " +
				"its audit entries were kept. Run 'skep-agent audit verify' on the host"
		default:
			f.Code = CodeArchiveFailed
			f.Message = "the audit archive could not be updated; see the agent log. No audit entry was lost"
		}
		return res, f
	}
	return res, nil
}

// Schedule calls submit once now and then every interval until ctx is
// done. It is a plain timer in the agent, not the cluster scheduler. A
// failed submission is logged and tried again at the next tick.
func Schedule(ctx context.Context, interval time.Duration, submit func(context.Context) error, log *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := submit(ctx); err != nil && ctx.Err() == nil {
			log.Warn("could not submit the retention task; it is tried again at the next interval", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
