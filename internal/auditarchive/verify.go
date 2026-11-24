package auditarchive

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Check statuses of a segment.
const (
	// StatusOK: the file is there and matches the index.
	StatusOK = "ok"
	// StatusMissing: the segment is in the index and its file is not in
	// the archive directory.
	StatusMissing = "missing"
	// StatusInvalid: the file, or the segment's place in the chain, does
	// not match the index.
	StatusInvalid = "invalid"
)

// Check is the result of verifying one segment.
type Check struct {
	Segment Segment `json:"segment"`
	Status  string  `json:"status"`
	// Detail says what is wrong when Status is not ok.
	Detail string `json:"detail,omitempty"`
}

// Report is the result of Verify.
type Report struct {
	// Checks has one entry per sealed segment, oldest first.
	Checks []Check `json:"checks"`
	// Unindexed lists files in the archive directory that look like
	// segments but are not in the index, for example one written by a run
	// that stopped before the seal. They are reported, never deleted.
	Unindexed []string `json:"unindexed,omitempty"`
}

// Missing returns the file names of the segments whose file is absent.
func (r Report) Missing() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Status == StatusMissing {
			out = append(out, c.Segment.FileName)
		}
	}
	return out
}

// Err returns the first break in the archive, oldest segment first: an
// error wrapping ErrInvalid, or ErrMissing unless allowMissing is set. It
// returns nil when there is none.
func (r Report) Err(allowMissing bool) error {
	for _, c := range r.Checks {
		switch {
		case c.Status == StatusInvalid:
			return fmt.Errorf("%w: segment %d (%s, %s): %s", ErrInvalid, c.Segment.Seq, c.Segment.Month, c.Segment.FileName, c.Detail)
		case c.Status == StatusMissing && !allowMissing:
			return fmt.Errorf("%w: segment %d (%s, %s)", ErrMissing, c.Segment.Seq, c.Segment.Month, c.Segment.FileName)
		}
	}
	return nil
}

// Verify walks the index of sealed segments and their files. For every
// segment it checks its place in the chain (numbering, ID range following
// the previous one, a later month, the previous segment's hash) and reads
// its file (structure, header, SHA-256). It changes nothing.
//
// The returned error is for failures to verify at all; what Verify found
// is in the Report.
func (a *Archiver) Verify(ctx context.Context) (Report, error) {
	segs, err := a.Segments(ctx)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	indexed := map[string]bool{}
	var prev Segment
	for i, seg := range segs {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		indexed[seg.FileName] = true
		c := Check{Segment: seg, Status: StatusOK}
		switch {
		case seg.Seq != int64(i)+1:
			c.Status, c.Detail = StatusInvalid, "the index is not numbered consecutively"
		case seg.FileName != FileName(seg.NodeID, seg.Month, seg.FirstID, seg.LastID):
			c.Status, c.Detail = StatusInvalid, "its file name does not match its node, month, and ID range"
		case i == 0 && seg.PreviousHash != "":
			c.Status, c.Detail = StatusInvalid, "the first segment names a predecessor"
		case i > 0 && seg.FirstID != prev.LastID+1:
			c.Status, c.Detail = StatusInvalid, "its ID range does not start right after the previous segment"
		case i > 0 && seg.Month <= prev.Month:
			c.Status, c.Detail = StatusInvalid, "its month is not after the previous segment's"
		case i > 0 && seg.PreviousHash != prev.Hash:
			c.Status, c.Detail = StatusInvalid, "it does not continue the hash chain"
		default:
			switch err := a.checkFile(seg); {
			case errors.Is(err, errMissing):
				c.Status, c.Detail = StatusMissing, "the file is not in the archive directory"
			case err != nil:
				c.Status, c.Detail = StatusInvalid, err.Error()
			}
		}
		rep.Checks = append(rep.Checks, c)
		prev = seg
	}

	entries, err := os.ReadDir(a.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, fmt.Errorf("auditarchive: read the archive directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "audit-") && strings.HasSuffix(name, ".jsonl.gz") && !indexed[name] {
			rep.Unindexed = append(rep.Unindexed, name)
		}
	}
	sort.Strings(rep.Unindexed)
	return rep, nil
}

// Export verifies the sealed segments and calls entry with every archived
// entry, as one line of JSON including the newline, oldest first. month
// limits it to one segment; empty means all. A segment that does not
// verify stops the export with ErrInvalid or ErrMissing.
func (a *Archiver) Export(ctx context.Context, month string, entry func(line []byte) error) (segments int, err error) {
	segs, err := a.Segments(ctx)
	if err != nil {
		return 0, err
	}
	for _, seg := range segs {
		if month != "" && seg.Month != month {
			continue
		}
		if err := ctx.Err(); err != nil {
			return segments, err
		}
		// Verify first, then emit: nothing of a file that does not match
		// its hash is ever printed.
		switch err := a.checkFile(seg); {
		case errors.Is(err, errMissing):
			return segments, fmt.Errorf("%w: segment %d (%s, %s)", ErrMissing, seg.Seq, seg.Month, seg.FileName)
		case err != nil:
			return segments, fmt.Errorf("%w: segment %d (%s, %s): %v", ErrInvalid, seg.Seq, seg.Month, seg.FileName, err)
		}
		if _, _, err := readSegment(filepath.Join(a.dir, seg.FileName), entry); err != nil {
			return segments, err
		}
		segments++
	}
	return segments, nil
}

// ExportUnsealed passes each live entry newer than the last sealed segment
// to entry, oldest first, in the format of a segment's lines, and returns
// how many there were. It only reads. An entry sealed while it runs may
// also appear in a later Export; none is skipped.
func (a *Archiver) ExportUnsealed(ctx context.Context, entry func(line []byte) error) (n int64, err error) {
	var first int64 = 1
	last, ok, err := a.lastSegment(ctx)
	if err != nil {
		return 0, err
	}
	if ok {
		first = last.LastID + 1
	}
	rows, err := a.queryRange(ctx, first, math.MaxInt64)
	if err != nil {
		return 0, fmt.Errorf("auditarchive: read the live entries: %w", err)
	}
	defer rows.rows.Close()
	var buf []byte
	for {
		_, values, more, err := rows.next()
		if err != nil {
			return n, fmt.Errorf("auditarchive: read the live entries: %w", err)
		}
		if !more {
			return n, nil
		}
		if buf, err = appendEntry(buf[:0], a.log.Columns, values); err != nil {
			return n, err
		}
		if err := entry(buf); err != nil {
			return n, err
		}
		n++
	}
}

// Summary is what one Cycle did.
type Summary struct {
	// Verified is the number of sealed segments whose file was checked
	// and found intact before anything else was done.
	Verified int `json:"segments_verified"`
	// Missing lists sealed segments whose file is not in the archive
	// directory. Moving segments to other storage is the operator's
	// choice, so this alone is not a failure; it is always reported.
	Missing []string `json:"segments_missing,omitempty"`
	// Unindexed lists segment-like files that are not in the index.
	Unindexed []string `json:"files_not_in_index,omitempty"`
	// Sealed lists the segments sealed by this run.
	Sealed []Segment `json:"sealed,omitempty"`
	// Removed says which live entries were removed.
	Removed Removed `json:"removed"`
}

// Cycle is one retention run for this log: verify the archive, seal every
// month that is due, then remove the live entries of segments older than
// liveDays (zero: never remove).
//
// If verification finds a break it seals and removes nothing and returns
// ErrInvalid. A missing file is reported in the summary; it becomes
// ErrMissing only when its segment's live entries are due for removal,
// and then nothing is removed. The outcome is remembered for Health.
func (a *Archiver) Cycle(ctx context.Context, liveDays int) (Summary, error) {
	sum, err := a.cycle(ctx, liveDays)
	if ctx.Err() == nil {
		// A run cut short by a shutdown says nothing about the archive.
		a.mu.Lock()
		a.lastErr = err
		a.healthOK = false
		a.mu.Unlock()
	}
	return sum, err
}

func (a *Archiver) cycle(ctx context.Context, liveDays int) (Summary, error) {
	var sum Summary
	rep, err := a.Verify(ctx)
	if err != nil {
		return sum, err
	}
	sum.Missing, sum.Unindexed = rep.Missing(), rep.Unindexed
	for _, c := range rep.Checks {
		if c.Status == StatusOK {
			sum.Verified++
		}
	}
	for _, name := range sum.Missing {
		a.logger.Warn("a sealed audit segment is not in the archive directory", "log", a.log.Name, "file", name)
	}
	for _, name := range sum.Unindexed {
		a.logger.Warn("a file in the audit archive directory is not a sealed segment", "log", a.log.Name, "file", name)
	}
	if err := rep.Err(true); err != nil {
		return sum, err
	}
	if sum.Sealed, err = a.Seal(ctx); err != nil {
		return sum, err
	}
	sum.Removed, err = a.Remove(ctx, liveDays)
	return sum, err
}

// lateSealDay is the day of the month from which an unsealed previous
// month is reported: 7 days into the new month.
const lateSealDay = 8

// healthTTL is how long Health reuses its answer.
const healthTTL = time.Minute

// Health reports what is wrong with the archive, or "" when nothing is.
// The text is safe to show to an authenticated caller; causes are in the
// log.
//
// It is degraded when the last Cycle failed verification or found a
// segment missing that it needed, and when the previous month is still
// unsealed 7 days into the new month. The second stays true until the
// seal succeeds.
func (a *Archiver) Health(ctx context.Context) string {
	now := a.now()
	a.mu.Lock()
	if a.healthOK && now.Sub(a.healthAt) < healthTTL && !now.Before(a.healthAt) {
		defer a.mu.Unlock()
		return a.health
	}
	lastErr := a.lastErr
	a.mu.Unlock()

	problem := ""
	switch {
	case errors.Is(lastErr, ErrInvalid):
		problem = "the audit archive failed verification; no audit entries are removed until it is repaired"
	case errors.Is(lastErr, ErrMissing):
		problem = "an audit archive segment is missing; its audit entries are kept in the database"
	default:
		late, err := a.Late(ctx)
		switch {
		case err != nil:
			a.logger.Error("health: could not check the audit archive", "log", a.log.Name, "error", err)
			problem = "the audit archive could not be checked"
		case late:
			problem = "last month's audit entries are not sealed yet"
		}
	}

	a.mu.Lock()
	a.health, a.healthAt, a.healthOK = problem, now, true
	a.mu.Unlock()
	return problem
}

// Late reports whether sealing is overdue: the calendar month before the
// current one has unsealed entries and the current month is at least 7
// days old.
func (a *Archiver) Late(ctx context.Context) (bool, error) {
	now := a.now().UTC()
	if now.Day() < lateSealDay {
		return false, nil
	}
	current := monthStart(now)
	previous := current.AddDate(0, -1, 0).Format(monthLayout)

	last, have, err := a.lastSegment(ctx)
	if err != nil {
		return false, err
	}
	if have && last.Month >= previous {
		// The previous month is sealed. Entries written afterwards with
		// an older timestamp belong to the next segment.
		return false, nil
	}
	var unsealed int
	if err := a.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM audit_log WHERE id > ? AND occurred_at < ?)`,
		last.LastID, current.Format(timeLayout)).Scan(&unsealed); err != nil {
		return false, fmt.Errorf("auditarchive: look for unsealed entries: %w", err)
	}
	return unsealed == 1, nil
}
