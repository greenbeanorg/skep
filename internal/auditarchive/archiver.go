package auditarchive

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Segment is one row of audit_segments: a sealed segment.
type Segment struct {
	Seq          int64     `json:"seq"`
	Month        string    `json:"month"`
	FirstID      int64     `json:"first_id"`
	LastID       int64     `json:"last_id"`
	Entries      int64     `json:"entries"`
	NodeID       string    `json:"node_id"`
	FileName     string    `json:"file_name"`
	Hash         string    `json:"hash"`
	PreviousHash string    `json:"previous_hash"`
	SealedAt     time.Time `json:"sealed_at"`
}

// Errors that say why the archive cannot be relied on. Both mean: nothing
// was removed, and the live entries are the only good copy.
var (
	// ErrInvalid is returned when the index or a segment file does not
	// verify: a broken chain, a changed or unreadable file.
	ErrInvalid = errors.New("the audit archive failed verification")
	// ErrMissing is returned when the file of a sealed segment is not in
	// the archive directory.
	ErrMissing = errors.New("an audit archive segment is missing")
)

// Options configures an Archiver.
type Options struct {
	// DB is the database that holds audit_log and audit_segments.
	DB *sql.DB
	// Log says which audit log DB holds.
	Log Log
	// Dir is the archive directory. It is created, mode 0700, when the
	// first segment is sealed; its parent must exist.
	Dir string
	// NodeID returns the ID of the local node, which is written into
	// segment names and headers. It is called only when a segment is
	// about to be sealed.
	NodeID func(ctx context.Context) (string, error)
	// Notify is called after each seal. Optional.
	Notify func(Segment)
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now. Tests replace it; nothing here reads the
	// system clock any other way.
	Now func() time.Time
}

// Archiver seals, verifies, and removes the segments of one audit log.
type Archiver struct {
	db     *sql.DB
	log    Log
	dir    string
	nodeID func(ctx context.Context) (string, error)
	notify func(Segment)
	logger *slog.Logger
	now    func() time.Time

	// fault, when set by a test, is called at the named points of Seal and
	// Remove. An error it returns is returned at once, with nothing
	// cleaned up: the state on disk and in the database is then what a
	// crash at that point leaves behind.
	fault func(point string) error

	mu sync.Mutex
	// lastErr is the outcome of the last Cycle, for the health check.
	lastErr error
	// health caches the health check's answer.
	health   string
	healthAt time.Time
	healthOK bool
}

// Fault points, in the order they are passed.
const (
	// faultTempWritten: the temporary file is complete and synced.
	faultTempWritten = "temp-written"
	// faultRenamed: the file has its final name; nothing is recorded.
	faultRenamed = "renamed"
	// faultSealed: the seal row is committed.
	faultSealed = "sealed"
	// faultRemoveBatch: one batch of live entries has been deleted.
	faultRemoveBatch = "remove-batch"
)

// tempPrefix marks files that are being written. They are never segments.
const tempPrefix = ".tmp-"

// removeBatch is the number of live entries deleted per transaction, so
// that no transaction holds the write lock for long.
const removeBatch = 500

// New returns an Archiver.
func New(opts Options) (*Archiver, error) {
	if opts.DB == nil {
		return nil, errors.New("auditarchive: a database is required")
	}
	if len(opts.Log.Columns) < 2 || opts.Log.Columns[0] != "id" || opts.Log.Columns[1] != "occurred_at" {
		return nil, errors.New("auditarchive: the log's columns must start with id and occurred_at")
	}
	if opts.Dir == "" {
		return nil, errors.New("auditarchive: an archive directory is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Archiver{
		db: opts.DB, log: opts.Log, dir: opts.Dir, nodeID: opts.NodeID, notify: opts.Notify,
		logger: opts.Logger, now: opts.Now,
	}, nil
}

// Dir returns the archive directory.
func (a *Archiver) Dir() string { return a.dir }

func (a *Archiver) at(point string) error {
	if a.fault == nil {
		return nil
	}
	return a.fault(point)
}

// Segments returns the index of sealed segments, oldest first.
func (a *Archiver) Segments(ctx context.Context) ([]Segment, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT seq, month, first_id, last_id, entry_count, node_id, file_name, hash, previous_hash, sealed_at
		FROM audit_segments ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("auditarchive: read the segment index: %w", err)
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var s Segment
		var sealed string
		if err := rows.Scan(&s.Seq, &s.Month, &s.FirstID, &s.LastID, &s.Entries, &s.NodeID,
			&s.FileName, &s.Hash, &s.PreviousHash, &sealed); err != nil {
			return nil, fmt.Errorf("auditarchive: read the segment index: %w", err)
		}
		if s.SealedAt, err = time.Parse(timeLayout, sealed); err != nil {
			return nil, fmt.Errorf("auditarchive: segment %d: %w", s.Seq, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auditarchive: read the segment index: %w", err)
	}
	return out, nil
}

// lastSegment returns the newest sealed segment, if any.
func (a *Archiver) lastSegment(ctx context.Context) (Segment, bool, error) {
	segs, err := a.Segments(ctx)
	if err != nil || len(segs) == 0 {
		return Segment{}, false, err
	}
	return segs[len(segs)-1], true, nil
}

// ensureDir creates the archive directory if needed and refuses one that
// somebody else owns or can enter.
func (a *Archiver) ensureDir() error {
	if err := os.Mkdir(a.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("auditarchive: create the archive directory: %w", err)
	}
	info, err := os.Lstat(a.dir)
	if err != nil {
		return fmt.Errorf("auditarchive: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("auditarchive: %s is not a directory", a.dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("auditarchive: %s has mode %04o; it must not be accessible to group or others (0700)",
			a.dir, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("auditarchive: %s is owned by uid %d, not by the uid this process runs as (%d)",
			a.dir, st.Uid, os.Geteuid())
	}
	return nil
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// dbRows adapts a query over a range of entries to rowSource.
type dbRows struct {
	rows   *sql.Rows
	values []any
	ptrs   []any
}

func (a *Archiver) queryRange(ctx context.Context, first, last int64) (*dbRows, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+strings.Join(a.log.Columns, ", ")+` FROM audit_log WHERE id BETWEEN ? AND ? ORDER BY id`,
		first, last)
	if err != nil {
		return nil, err
	}
	r := &dbRows{rows: rows, values: make([]any, len(a.log.Columns)), ptrs: make([]any, len(a.log.Columns))}
	for i := range r.values {
		r.ptrs[i] = &r.values[i]
	}
	return r, nil
}

func (r *dbRows) next() (int64, []any, bool, error) {
	if !r.rows.Next() {
		return 0, nil, false, r.rows.Err()
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		return 0, nil, false, err
	}
	id, ok := r.values[0].(int64)
	if !ok {
		return 0, nil, false, fmt.Errorf("id has unexpected type %T", r.values[0])
	}
	return id, r.values, true, nil
}

// Seal writes and records a segment for every calendar month (UTC) that
// has ended and still has unsealed entries, oldest first, and returns the
// segments it sealed. The entries stay in the live table.
//
// It is idempotent, and safe to interrupt at any point: a file that was
// written but not recorded is not a segment, and is written again.
func (a *Archiver) Seal(ctx context.Context) ([]Segment, error) {
	var sealed []Segment
	for {
		seg, ok, err := a.sealNext(ctx)
		if err != nil {
			return sealed, err
		}
		if !ok {
			return sealed, nil
		}
		sealed = append(sealed, seg)
	}
}

// sealNext seals the oldest month that is due, if there is one.
func (a *Archiver) sealNext(ctx context.Context) (Segment, bool, error) {
	prev, havePrev, err := a.lastSegment(ctx)
	if err != nil {
		return Segment{}, false, err
	}

	// The range starts at the ID after the previous segment, or at the
	// first entry of the log.
	first := prev.LastID + 1
	if !havePrev {
		var min sql.NullInt64
		if err := a.db.QueryRowContext(ctx, `SELECT MIN(id) FROM audit_log`).Scan(&min); err != nil {
			return Segment{}, false, fmt.Errorf("auditarchive: find the first entry: %w", err)
		}
		if !min.Valid {
			return Segment{}, false, nil // an empty log
		}
		first = min.Int64
	}

	// The month is the one of the oldest unsealed timestamp, but always
	// after the previous segment's: an entry written late with an old
	// timestamp belongs to the next segment, because its ID says so.
	var oldest sql.NullString
	if err := a.db.QueryRowContext(ctx, `SELECT MIN(occurred_at) FROM audit_log WHERE id >= ?`, first).Scan(&oldest); err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: find the oldest unsealed entry: %w", err)
	}
	if !oldest.Valid {
		return Segment{}, false, nil // everything is sealed
	}
	oldestAt, err := time.Parse(timeLayout, oldest.String)
	if err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: an unsealed entry has an unreadable timestamp: %w", err)
	}
	month := monthStart(oldestAt)
	if havePrev {
		prevMonth, err := parseMonth(prev.Month)
		if err != nil {
			return Segment{}, false, fmt.Errorf("auditarchive: segment %d: %w", prev.Seq, err)
		}
		if !month.After(prevMonth) {
			month = nextMonth(prevMonth)
		}
	}
	end := nextMonth(month)
	if a.now().UTC().Before(end) {
		return Segment{}, false, nil // that month has not ended
	}

	// The boundary: the highest ID whose timestamp is before the end of
	// the month, as found now. Everything up to it belongs to the segment,
	// whatever its own timestamp says.
	var (
		last             sql.NullInt64
		count            int64
		earliest, latest sql.NullString
	)
	if err := a.db.QueryRowContext(ctx,
		`SELECT MAX(id) FROM audit_log WHERE id >= ? AND occurred_at < ?`,
		first, end.Format(timeLayout)).Scan(&last); err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: find the end of %s: %w", month.Format(monthLayout), err)
	}
	if !last.Valid {
		return Segment{}, false, nil
	}
	if err := a.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(occurred_at), MAX(occurred_at) FROM audit_log WHERE id BETWEEN ? AND ?`,
		first, last.Int64).Scan(&count, &earliest, &latest); err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: measure %s: %w", month.Format(monthLayout), err)
	}

	if a.nodeID == nil {
		return Segment{}, false, errors.New("auditarchive: no node ID source is configured")
	}
	nodeID, err := a.nodeID(ctx)
	if err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: the node ID is not known yet: %w", err)
	}
	if !nodeIDRE.MatchString(nodeID) {
		return Segment{}, false, fmt.Errorf("auditarchive: node ID %q cannot be part of a file name", nodeID)
	}

	seg := Segment{
		Seq:          prev.Seq + 1,
		Month:        month.Format(monthLayout),
		FirstID:      first,
		LastID:       last.Int64,
		Entries:      count,
		NodeID:       nodeID,
		PreviousHash: prev.Hash,
	}
	seg.FileName = FileName(nodeID, seg.Month, seg.FirstID, seg.LastID)
	header := Header{
		Format: FormatVersion, Log: a.log.Name, NodeID: nodeID, Month: seg.Month,
		FirstID: seg.FirstID, LastID: seg.LastID, Entries: seg.Entries,
		Earliest: earliest.String, Latest: latest.String, PreviousHash: seg.PreviousHash,
	}

	if err := a.ensureDir(); err != nil {
		return Segment{}, false, err
	}
	a.removeStaleTemps()
	if seg.Hash, err = a.writeFile(ctx, seg, header); err != nil {
		return Segment{}, false, err
	}

	// Read the file back and compare it with the live table before
	// anything is recorded: the same header and entries, written again
	// from the table, must give the hash of what is on disk.
	if err := a.compareWithLog(ctx, seg, header); err != nil {
		return Segment{}, false, err
	}

	// The seal. The database checks once more that the range follows the
	// previous segment, continues the chain, and matches the table.
	seg.SealedAt = a.now().UTC().Truncate(time.Millisecond)
	if _, err := a.db.ExecContext(ctx, `
		INSERT INTO audit_segments
			(seq, month, first_id, last_id, entry_count, node_id, file_name, hash, previous_hash, sealed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seg.Seq, seg.Month, seg.FirstID, seg.LastID, seg.Entries, seg.NodeID, seg.FileName,
		seg.Hash, seg.PreviousHash, seg.SealedAt.Format(timeLayout)); err != nil {
		return Segment{}, false, fmt.Errorf("auditarchive: record the seal of %s: %w", seg.Month, err)
	}
	if err := a.at(faultSealed); err != nil {
		return Segment{}, false, err
	}

	a.logger.Info("audit segment sealed", "log", a.log.Name, "month", seg.Month,
		"first_id", seg.FirstID, "last_id", seg.LastID, "entries", seg.Entries,
		"sha256", seg.Hash, "file", seg.FileName)
	if a.notify != nil {
		a.notify(seg)
	}
	return seg, true, nil
}

// writeFile writes the segment to a temporary name, syncs it, and renames
// it into place. It returns the hash of the uncompressed content.
func (a *Archiver) writeFile(ctx context.Context, seg Segment, header Header) (string, error) {
	final := filepath.Join(a.dir, seg.FileName)
	temp := filepath.Join(a.dir, tempPrefix+seg.FileName)
	fail := func(err error) (string, error) {
		os.Remove(temp)
		return "", fmt.Errorf("auditarchive: write %s: %w", seg.FileName, err)
	}

	// 0400 from the start: the file is written through this descriptor
	// and never opened for writing again.
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fail(err)
	}
	src, err := a.queryRange(ctx, seg.FirstID, seg.LastID)
	if err != nil {
		f.Close()
		return fail(err)
	}
	zw := gzip.NewWriter(f)
	hash, err := writeSegment(zw, a.log, header, src)
	err = errors.Join(err, src.rows.Close(), zw.Close())
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fail(err)
	}
	if err := a.at(faultTempWritten); err != nil {
		return "", err
	}

	// A file with the final name that is not in the index was written by
	// a run that stopped before the seal. It is not a segment; this one
	// takes its place. A name that is in the index is never replaced.
	var indexed int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_segments WHERE file_name = ?`, seg.FileName).Scan(&indexed); err != nil {
		return fail(err)
	}
	if indexed != 0 {
		return fail(errors.New("a sealed segment with this name already exists"))
	}
	if err := os.Rename(temp, final); err != nil {
		return fail(err)
	}
	if err := syncDir(a.dir); err != nil {
		return "", fmt.Errorf("auditarchive: sync %s: %w", a.dir, err)
	}
	if err := a.at(faultRenamed); err != nil {
		return "", err
	}
	return hash, nil
}

// compareWithLog reads the segment file back and checks it against its
// description and against the live table.
func (a *Archiver) compareWithLog(ctx context.Context, seg Segment, header Header) error {
	if err := a.checkFile(seg); err != nil {
		return fmt.Errorf("auditarchive: %s does not read back as written: %w", seg.FileName, err)
	}
	src, err := a.queryRange(ctx, seg.FirstID, seg.LastID)
	if err != nil {
		return fmt.Errorf("auditarchive: re-read %s: %w", seg.Month, err)
	}
	defer src.rows.Close()
	fromLog, err := writeSegment(discard{}, a.log, header, src)
	if err != nil {
		return fmt.Errorf("auditarchive: re-read %s: %w", seg.Month, err)
	}
	if fromLog != seg.Hash {
		return fmt.Errorf("auditarchive: %s does not match the live table; nothing was recorded", seg.FileName)
	}
	return nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// checkFile reads the file of a sealed segment and compares it with the
// index: structure, header, and hash. It returns errMissing when the file
// is not there.
func (a *Archiver) checkFile(seg Segment) error {
	h, hash, err := readSegment(filepath.Join(a.dir, seg.FileName), nil)
	if err != nil {
		return err
	}
	want := Header{
		Format: FormatVersion, Log: a.log.Name, NodeID: seg.NodeID, Month: seg.Month,
		FirstID: seg.FirstID, LastID: seg.LastID, Entries: seg.Entries,
		Earliest: h.Earliest, Latest: h.Latest, PreviousHash: seg.PreviousHash,
	}
	if h != want {
		return errors.New("its header does not match the index")
	}
	if hash != seg.Hash {
		return fmt.Errorf("its SHA-256 is %s, the index says %s", hash, seg.Hash)
	}
	return nil
}

// removeStaleTemps deletes temporary files left by an interrupted seal.
// They are never segments: a segment has its final name.
func (a *Archiver) removeStaleTemps() {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) && e.Type().IsRegular() {
			if err := os.Remove(filepath.Join(a.dir, e.Name())); err == nil {
				a.logger.Warn("removed an unfinished audit segment file left by an interrupted run", "file", e.Name())
			}
		}
	}
}

// Removed says what Remove took out of the live table.
type Removed struct {
	// Months lists the segments whose live entries were removed
	// completely by this call.
	Months []string `json:"months,omitempty"`
	// Entries is the number of live entries deleted.
	Entries int64 `json:"entries"`
}

// Remove deletes the live entries of every sealed segment whose month
// ended more than liveDays days ago, oldest first. Before it touches a
// segment's entries it reads the segment file again; if the file is
// missing or does not match the index it removes nothing of that segment
// or of any later one and returns ErrMissing or ErrInvalid.
//
// liveDays of zero or less removes nothing. Interrupted, it leaves a
// segment partly removed; the next call removes the rest.
func (a *Archiver) Remove(ctx context.Context, liveDays int) (Removed, error) {
	var out Removed
	if liveDays <= 0 {
		return out, nil
	}
	segs, err := a.Segments(ctx)
	if err != nil {
		return out, err
	}
	now := a.now().UTC()
	for _, seg := range segs {
		month, err := parseMonth(seg.Month)
		if err != nil {
			return out, fmt.Errorf("auditarchive: segment %d: %w", seg.Seq, err)
		}
		if now.Before(nextMonth(month).AddDate(0, 0, liveDays)) {
			break // later segments are younger still
		}
		var live int64
		if err := a.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_log WHERE id BETWEEN ? AND ?`, seg.FirstID, seg.LastID).Scan(&live); err != nil {
			return out, fmt.Errorf("auditarchive: count the live entries of %s: %w", seg.Month, err)
		}
		if live == 0 {
			continue // removed before
		}

		switch err := a.checkFile(seg); {
		case errors.Is(err, errMissing):
			return out, fmt.Errorf("%w: %s (%s); its %d live entries were not removed", ErrMissing, seg.FileName, seg.Month, live)
		case err != nil:
			return out, fmt.Errorf("%w: %s: %v; its %d live entries were not removed", ErrInvalid, seg.FileName, err, live)
		}

		for {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			res, err := a.db.ExecContext(ctx, `
				DELETE FROM audit_log WHERE id IN (
					SELECT id FROM audit_log WHERE id BETWEEN ? AND ? ORDER BY id LIMIT ?)`,
				seg.FirstID, seg.LastID, removeBatch)
			if err != nil {
				return out, fmt.Errorf("auditarchive: remove the live entries of %s: %w", seg.Month, err)
			}
			n, _ := res.RowsAffected()
			out.Entries += n
			if n == 0 {
				break
			}
			if err := a.at(faultRemoveBatch); err != nil {
				return out, err
			}
		}
		out.Months = append(out.Months, seg.Month)
		a.logger.Info("live audit entries removed; they remain in the archive", "log", a.log.Name,
			"month", seg.Month, "first_id", seg.FirstID, "last_id", seg.LastID, "file", seg.FileName)
	}
	return out, nil
}
