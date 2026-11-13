package auditarchive

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
)

const testNode = "97fc6968-7e82-49c9-bda8-9026775ca964"

// clock is the injected clock. No test changes the system clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) set(s string) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	c.t = t
}

// kind is one of the two audit logs; most tests run against both.
type kind struct {
	log        Log
	migrations func() fs.FS
	insert     string // arguments: occurred_at, details
}

var kinds = []kind{
	{AgentLog, migrations.Agent,
		`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome, details) VALUES (?, 'u1', 'andy', 1001, 'x.y', 'success', ?)`},
	{AccessLog, migrations.API,
		`INSERT INTO audit_log (occurred_at, actor_name, action, outcome, details) VALUES (?, 'andy', 'auth.login', 'success', ?)`},
}

type env struct {
	t     *testing.T
	kind  kind
	db    *sql.DB
	dir   string
	clock *clock
	a     *Archiver
}

func newEnv(t *testing.T, k kind) *env {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	db, err := database.Open(context.Background(), filepath.Join(state, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(context.Background(), db, k.migrations()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, kind: k, db: db, dir: filepath.Join(state, "audit-archive"), clock: &clock{}}
	e.clock.set("2026-11-12T12:00:00Z")
	e.restart()
	return e
}

// restart replaces the archiver with a new one on the same database and
// directory, as a restarted process would.
func (e *env) restart() {
	e.t.Helper()
	a, err := New(Options{
		DB: e.db, Log: e.kind.log, Dir: e.dir,
		NodeID: func(context.Context) (string, error) { return testNode, nil },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    e.clock.now,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.a = a
}

// add appends n entries stamped at, a timestamp like 2026-09-10T12:00:00.000Z.
func (e *env) add(at string, n int) {
	e.t.Helper()
	for range n {
		if _, err := e.db.Exec(e.kind.insert, at, `{"note":"x"}`); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) liveIDs() []int64 {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT id FROM audit_log ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *env) segments() []Segment {
	e.t.Helper()
	segs, err := e.a.Segments(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return segs
}

// ranges renders the index as "month:first-last ...".
func (e *env) ranges() string {
	var parts []string
	for _, s := range e.segments() {
		parts = append(parts, fmt.Sprintf("%s:%d-%d", s.Month, s.FirstID, s.LastID))
	}
	return strings.Join(parts, " ")
}

func (e *env) seal() []Segment {
	e.t.Helper()
	segs, err := e.a.Seal(context.Background())
	if err != nil {
		e.t.Fatalf("Seal: %v", err)
	}
	return segs
}

func (e *env) files() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, f := range entries {
		names = append(names, f.Name())
	}
	return names
}

// verifyOK fails the test unless every segment verifies and no stray file
// is in the directory.
func (e *env) verifyOK() {
	e.t.Helper()
	rep, err := e.a.Verify(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := rep.Err(false); err != nil {
		e.t.Fatalf("Verify: %v", err)
	}
	if len(rep.Unindexed) != 0 {
		e.t.Fatalf("files not in the index: %v", rep.Unindexed)
	}
}

// wantProtected checks that the database still refuses to delete id.
func (e *env) wantProtected(id int64) {
	e.t.Helper()
	_, err := e.db.Exec(`DELETE FROM audit_log WHERE id = ?`, id)
	if err == nil || !strings.Contains(err.Error(), "only after it is archived") {
		e.t.Fatalf("delete of unsealed entry %d: %v, want it refused by the trigger", id, err)
	}
}

func forKinds(t *testing.T, fn func(t *testing.T, e *env)) {
	for _, k := range kinds {
		t.Run(k.log.Name, func(t *testing.T) { fn(t, newEnv(t, k)) })
	}
}

// rewrite replaces the uncompressed content of a segment file.
func rewrite(t *testing.T, path string, change func([]byte) []byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(zr)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(change(content))
	zw.Close()
	overwrite(t, path, out.Bytes())
}

func overwrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}

func TestSealMonthBoundaries(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-09-10T12:00:00.000Z", 3) // 1-3
		e.add("2026-09-30T23:59:59.999Z", 1) // 4: the last instant of September
		e.add("2026-10-01T00:00:00.000Z", 1) // 5: the first instant of October
		e.add("2026-10-20T08:00:00.000Z", 2) // 6-7
		e.add("2026-11-02T08:00:00.000Z", 2) // 8-9

		// One millisecond before October ends: September is due, October is not.
		e.clock.set("2026-10-31T23:59:59.999Z")
		if got := e.seal(); len(got) != 1 || got[0].Month != "2026-09" {
			t.Fatalf("sealed %+v, want September only", got)
		}
		if got := e.ranges(); got != "2026-09:1-4" {
			t.Fatalf("index = %q", got)
		}

		// The month has turned.
		e.clock.set("2026-11-01T00:00:00Z")
		if got := e.seal(); len(got) != 1 || got[0].Month != "2026-10" {
			t.Fatalf("sealed %+v, want October only", got)
		}
		if got := e.ranges(); got != "2026-09:1-4 2026-10:5-7" {
			t.Fatalf("index = %q", got)
		}

		// Sealing is idempotent, and the current month is never sealed.
		e.clock.set("2026-11-30T23:59:59.999Z")
		if got := e.seal(); len(got) != 0 {
			t.Fatalf("a second run sealed %+v", got)
		}

		// Sealing removes nothing.
		if ids := e.liveIDs(); len(ids) != 9 {
			t.Fatalf("%d live entries after sealing, want all 9", len(ids))
		}
		e.verifyOK()

		// The chain, the names, and the permissions.
		segs := e.segments()
		if segs[0].PreviousHash != "" || segs[1].PreviousHash != segs[0].Hash || segs[0].Hash == segs[1].Hash {
			t.Errorf("hash chain: %+v", segs)
		}
		if want := "audit-" + testNode + "-2026-10-5-7.jsonl.gz"; segs[1].FileName != want {
			t.Errorf("file name = %q, want %q", segs[1].FileName, want)
		}
		if info, err := os.Stat(e.dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("archive directory mode = %v, %v; want 0700", info.Mode().Perm(), err)
		}
		for _, s := range segs {
			info, err := os.Stat(filepath.Join(e.dir, s.FileName))
			if err != nil || info.Mode().Perm() != 0o400 {
				t.Errorf("%s: mode %v, %v; want 0400", s.FileName, info.Mode().Perm(), err)
			}
		}
		if got := e.files(); len(got) != 2 {
			t.Errorf("files = %v, want the two segments and nothing else", got)
		}

		// The header carries the informational timestamps.
		h, _, err := readSegment(filepath.Join(e.dir, segs[0].FileName), nil)
		if err != nil {
			t.Fatal(err)
		}
		if h.Log != e.kind.log.Name || h.NodeID != testNode || h.Entries != 4 ||
			h.Earliest != "2026-09-10T12:00:00.000Z" || h.Latest != "2026-09-30T23:59:59.999Z" {
			t.Errorf("header = %+v", h)
		}
	})
}

func TestSealAcrossYearsAndEmptyMonths(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-08-15T00:00:00.000Z", 2) // 1-2
		// Nothing in September, October, or November.
		e.add("2026-12-31T23:59:59.999Z", 1) // 3
		e.add("2027-01-01T00:00:00.000Z", 1) // 4

		e.clock.set("2026-12-31T23:59:59.999Z")
		e.seal()
		if got := e.ranges(); got != "2026-08:1-2" {
			t.Fatalf("index = %q", got)
		}
		e.clock.set("2027-01-01T00:00:00Z")
		e.seal()
		// A month with no entries gets no segment.
		if got := e.ranges(); got != "2026-08:1-2 2026-12:3-3" {
			t.Fatalf("index = %q", got)
		}
		e.clock.set("2027-02-01T00:00:00Z")
		e.seal()
		if got := e.ranges(); got != "2026-08:1-2 2026-12:3-3 2027-01:4-4" {
			t.Fatalf("index = %q", got)
		}
		e.verifyOK()
	})
}

// TestMembershipIsByID checks amendment (c): the entry ID decides which
// segment an entry belongs to, so a skewed clock cannot put an entry in
// two segments, in none, or out of order.
func TestMembershipIsByID(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-09-10T12:00:00.000Z", 1) // 1
		e.add("2026-10-02T12:00:00.000Z", 1) // 2: the clock ran ahead
		e.add("2026-09-20T12:00:00.000Z", 1) // 3: and was put back
		e.add("2026-10-05T12:00:00.000Z", 1) // 4
		e.clock.set("2026-11-01T00:00:00Z")
		e.seal()
		// September ends at the highest ID stamped before October: 3. Entry
		// 2 lies inside that range and belongs to it, whatever its stamp.
		if got := e.ranges(); got != "2026-09:1-3 2026-10:4-4" {
			t.Fatalf("index = %q", got)
		}
		h, _, err := readSegment(filepath.Join(e.dir, e.segments()[0].FileName), nil)
		if err != nil {
			t.Fatal(err)
		}
		if h.Latest != "2026-10-02T12:00:00.000Z" {
			t.Errorf("latest = %q: a timestamp outside the month is recorded, not an error", h.Latest)
		}

		// An entry written after its month was sealed, with an old stamp,
		// is not lost and does not reopen the old segment: it is in the
		// next one.
		e.add("2026-09-25T12:00:00.000Z", 1) // 5
		e.add("2026-11-03T12:00:00.000Z", 1) // 6
		// It also does not make sealing look late: October is sealed.
		e.clock.set("2026-11-20T00:00:00Z")
		if late, err := e.a.Late(context.Background()); err != nil || late {
			t.Errorf("Late = %v, %v; want false: the previous month is sealed", late, err)
		}
		e.clock.set("2026-12-01T00:00:00Z")
		e.seal()
		if got := e.ranges(); got != "2026-09:1-3 2026-10:4-4 2026-11:5-6" {
			t.Fatalf("index = %q", got)
		}
		e.verifyOK()

		// Every entry is in exactly one segment, in ID order.
		var exported []int64
		if _, err := e.a.Export(context.Background(), "", func(line []byte) error {
			var v struct{ ID int64 }
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			exported = append(exported, v.ID)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(exported) != "[1 2 3 4 5 6]" {
			t.Errorf("archived IDs = %v", exported)
		}
	})
}

var errCrash = errors.New("simulated crash")

// crashAt makes the archiver stop at point, the n-th time it is reached.
func (e *env) crashAt(point string, nth int) {
	seen := 0
	e.a.fault = func(p string) error {
		if p == point {
			if seen++; seen == nth {
				return errCrash
			}
		}
		return nil
	}
}

// TestSealSurvivesInterruption stops a seal after the file is written,
// after it is renamed, and after the seal is recorded. Whatever the point,
// the entries stay protected, and the next run ends in exactly the state
// an uninterrupted run reaches.
func TestSealSurvivesInterruption(t *testing.T) {
	fill := func(e *env) {
		e.add("2026-09-10T12:00:00.000Z", 5) // 1-5
		e.add("2026-10-10T12:00:00.000Z", 5) // 6-10
		e.add("2026-11-10T12:00:00.000Z", 1) // 11
		e.clock.set("2026-11-12T12:00:00Z")
	}
	for _, k := range kinds {
		control := newEnv(t, k)
		fill(control)
		control.seal()
		want := control.segments()

		for _, tc := range []struct {
			point string
			// What the crash leaves behind.
			files    []string
			segments int
		}{
			{faultTempWritten, []string{tempPrefix + want[0].FileName}, 0},
			{faultRenamed, []string{want[0].FileName}, 0},
			{faultSealed, []string{want[0].FileName}, 1},
		} {
			t.Run(k.log.Name+"/"+tc.point, func(t *testing.T) {
				e := newEnv(t, k)
				fill(e)
				e.crashAt(tc.point, 1)
				if _, err := e.a.Seal(context.Background()); !errors.Is(err, errCrash) {
					t.Fatalf("Seal = %v, want the simulated crash", err)
				}
				if got := e.files(); fmt.Sprint(got) != fmt.Sprint(tc.files) {
					t.Fatalf("files after the crash = %v, want %v", got, tc.files)
				}
				if got := len(e.segments()); got != tc.segments {
					t.Fatalf("%d segments recorded after the crash, want %d", got, tc.segments)
				}
				// A file without a seal row protects nothing and permits
				// nothing: every entry not in a recorded segment is still
				// undeletable, and nothing was removed.
				if len(e.liveIDs()) != 11 {
					t.Fatalf("entries were lost: %v", e.liveIDs())
				}
				firstUnsealed := int64(1)
				if tc.segments == 1 {
					firstUnsealed = 6
				}
				e.wantProtected(firstUnsealed)
				e.wantProtected(11)

				// Verification of the half-finished state reports the stray
				// file and finds no broken segment.
				rep, err := e.a.Verify(context.Background())
				if err != nil || rep.Err(false) != nil {
					t.Fatalf("Verify after the crash: %v, %v", err, rep.Err(false))
				}
				if tc.point == faultRenamed && fmt.Sprint(rep.Unindexed) != fmt.Sprint(tc.files) {
					t.Errorf("unindexed = %v, want the unrecorded file", rep.Unindexed)
				}

				// The process starts again.
				e.restart()
				e.seal()
				got := e.segments()
				if len(got) != len(want) {
					t.Fatalf("%d segments after recovery, want %d", len(got), len(want))
				}
				for i := range want {
					g, w := got[i], want[i]
					g.SealedAt, w.SealedAt = time.Time{}, time.Time{}
					if g != w {
						t.Errorf("segment %d after recovery = %+v\nwant %+v", i+1, g, w)
					}
				}
				if files := e.files(); len(files) != len(want) {
					t.Errorf("files after recovery = %v, want only the %d segments", files, len(want))
				}
				e.verifyOK()
				if again := e.seal(); len(again) != 0 {
					t.Errorf("another run sealed %+v", again)
				}
			})
		}
	}
}

// TestUnrecordedFileIsNeverDeleted covers the one case in which the file
// of an interrupted seal is not simply rewritten: an entry with an old
// timestamp arrived in between, so the range, and with it the name, grew.
// The stray file is reported and left alone.
func TestUnrecordedFileIsNeverDeleted(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-10-10T12:00:00.000Z", 3)
		e.clock.set("2026-11-02T00:00:00Z")
		e.crashAt(faultRenamed, 1)
		if _, err := e.a.Seal(context.Background()); !errors.Is(err, errCrash) {
			t.Fatal(err)
		}
		stray := e.files()[0]
		e.add("2026-10-31T23:00:00.000Z", 1) // 4, written after the month turned

		e.restart()
		e.seal()
		if got := e.ranges(); got != "2026-10:1-4" {
			t.Fatalf("index = %q", got)
		}
		rep, err := e.a.Verify(context.Background())
		if err != nil || rep.Err(false) != nil {
			t.Fatalf("Verify: %v, %v", err, rep.Err(false))
		}
		if len(rep.Unindexed) != 1 || rep.Unindexed[0] != stray {
			t.Errorf("unindexed = %v, want %q", rep.Unindexed, stray)
		}
		if _, err := os.Stat(filepath.Join(e.dir, stray)); err != nil {
			t.Errorf("the stray file was deleted: %v", err)
		}
	})
}

func TestRemoveByAge(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		e.add("2026-09-10T12:00:00.000Z", 4) // 1-4
		e.add("2026-10-10T12:00:00.000Z", 3) // 5-7
		e.add("2026-11-10T12:00:00.000Z", 2) // 8-9
		e.clock.set("2026-11-01T00:00:00Z")
		e.seal()

		// September ended on October 1st; 35 days later is November 5th.
		e.clock.set("2026-11-04T23:59:59.999Z")
		if r, err := e.a.Remove(ctx, 35); err != nil || r.Entries != 0 {
			t.Fatalf("Remove one millisecond early = %+v, %v", r, err)
		}
		e.clock.set("2026-11-05T00:00:00Z")
		r, err := e.a.Remove(ctx, 35)
		if err != nil || r.Entries != 4 || fmt.Sprint(r.Months) != "[2026-09]" {
			t.Fatalf("Remove = %+v, %v; want the 4 entries of September", r, err)
		}
		if got := fmt.Sprint(e.liveIDs()); got != "[5 6 7 8 9]" {
			t.Fatalf("live IDs = %s", got)
		}
		if r, err := e.a.Remove(ctx, 35); err != nil || r.Entries != 0 || len(r.Months) != 0 {
			t.Fatalf("a second Remove = %+v, %v", r, err)
		}

		// What left the table is still in the archive, and the archive
		// still verifies without its live rows.
		e.verifyOK()
		var archived int
		if n, err := e.a.Export(ctx, "2026-09", func([]byte) error { archived++; return nil }); err != nil || n != 1 || archived != 4 {
			t.Fatalf("Export of September: %d segments, %d entries, %v", n, archived, err)
		}

		// Zero means never, however old the segment is. This is the
		// agent's default.
		e.clock.set("2036-01-01T00:00:00Z")
		if r, err := e.a.Remove(ctx, 0); err != nil || r.Entries != 0 {
			t.Fatalf("Remove with 0 days = %+v, %v", r, err)
		}
		if got := fmt.Sprint(e.liveIDs()); got != "[5 6 7 8 9]" {
			t.Fatalf("live IDs = %s", got)
		}
		// Unsealed entries are never removed, at any age.
		if r, err := e.a.Remove(ctx, 35); err != nil || r.Entries != 3 {
			t.Fatalf("Remove = %+v, %v; want October", r, err)
		}
		if got := fmt.Sprint(e.liveIDs()); got != "[8 9]" {
			t.Fatalf("live IDs = %s: the unsealed entries of November must stay", got)
		}
	})
}

// TestRemoveSurvivesInterruption stops a removal between two batches.
func TestRemoveSurvivesInterruption(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		const n = 2*removeBatch + 200
		tx, err := e.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for range n {
			if _, err := tx.Exec(e.kind.insert, "2026-09-10T12:00:00.000Z", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		e.add("2026-11-10T12:00:00.000Z", 2)
		e.clock.set("2026-11-12T00:00:00Z")
		e.seal()

		e.crashAt(faultRemoveBatch, 2)
		r, err := e.a.Remove(ctx, 35)
		if !errors.Is(err, errCrash) || r.Entries != 2*removeBatch {
			t.Fatalf("Remove = %+v, %v; want the crash after two batches", r, err)
		}
		if got := len(e.liveIDs()); got != 200+2 {
			t.Fatalf("%d live entries after the crash, want 202", got)
		}
		// A half-removed segment is still a sealed, intact segment.
		e.verifyOK()
		e.wantProtected(n + 1)

		e.restart()
		r, err = e.a.Remove(ctx, 35)
		if err != nil || r.Entries != 200 || fmt.Sprint(r.Months) != "[2026-09]" {
			t.Fatalf("Remove after restart = %+v, %v; want the remaining 200", r, err)
		}
		if got := fmt.Sprint(e.liveIDs()); got != fmt.Sprintf("[%d %d]", n+1, n+2) {
			t.Fatalf("live IDs = %s", got)
		}
		var archived int
		if _, err := e.a.Export(ctx, "", func([]byte) error { archived++; return nil }); err != nil || archived != n {
			t.Fatalf("archived entries = %d, %v; want %d", archived, err, n)
		}
	})
}

// damage is a way an archive file can stop matching the index.
type damage struct {
	name string
	do   func(t *testing.T, e *env, seg Segment)
	want error
}

var damages = []damage{
	{"missing", func(t *testing.T, e *env, seg Segment) {
		if err := os.Remove(filepath.Join(e.dir, seg.FileName)); err != nil {
			t.Fatal(err)
		}
	}, ErrMissing},
	{"an entry changed", func(t *testing.T, e *env, seg Segment) {
		rewrite(t, filepath.Join(e.dir, seg.FileName), func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"andy"`), []byte(`"eve!"`), 1)
		})
	}, ErrInvalid},
	{"an entry dropped", func(t *testing.T, e *env, seg Segment) {
		rewrite(t, filepath.Join(e.dir, seg.FileName), func(b []byte) []byte {
			lines := bytes.SplitAfter(b, []byte("\n"))
			return bytes.Join(append(lines[:1:1], lines[2:]...), nil)
		})
	}, ErrInvalid},
	{"an entry added", func(t *testing.T, e *env, seg Segment) {
		rewrite(t, filepath.Join(e.dir, seg.FileName), func(b []byte) []byte {
			return append(b, []byte(fmt.Sprintf("{\"id\":%d}\n", seg.LastID))...)
		})
	}, ErrInvalid},
	{"truncated", func(t *testing.T, e *env, seg Segment) {
		path := filepath.Join(e.dir, seg.FileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		overwrite(t, path, data[:len(data)/2])
	}, ErrInvalid},
	{"data appended", func(t *testing.T, e *env, seg Segment) {
		path := filepath.Join(e.dir, seg.FileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		overwrite(t, path, append(data, data...))
	}, ErrInvalid},
	{"empty", func(t *testing.T, e *env, seg Segment) {
		overwrite(t, filepath.Join(e.dir, seg.FileName), nil)
	}, ErrInvalid},
	{"replaced by a directory", func(t *testing.T, e *env, seg Segment) {
		path := filepath.Join(e.dir, seg.FileName)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}, ErrInvalid},
	{"replaced by a link to another segment", func(t *testing.T, e *env, seg Segment) {
		path := filepath.Join(e.dir, seg.FileName)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e.segments()[1].FileName, path); err != nil {
			t.Fatal(err)
		}
	}, ErrInvalid},
	{"replaced by a copy of another segment", func(t *testing.T, e *env, seg Segment) {
		other, err := os.ReadFile(filepath.Join(e.dir, e.segments()[1].FileName))
		if err != nil {
			t.Fatal(err)
		}
		overwrite(t, filepath.Join(e.dir, seg.FileName), other)
	}, ErrInvalid},
}

// TestRemoveRefusesMissingOrAlteredFile checks that live entries stay when
// their archive copy cannot be trusted: nothing is removed, the run fails
// with a specific error, and the health check says so until it is fixed.
func TestRemoveRefusesMissingOrAlteredFile(t *testing.T) {
	for _, k := range kinds {
		for _, d := range damages {
			t.Run(k.log.Name+"/"+d.name, func(t *testing.T) {
				ctx := context.Background()
				e := newEnv(t, k)
				e.add("2026-08-10T12:00:00.000Z", 3) // 1-3
				e.add("2026-09-10T12:00:00.000Z", 3) // 4-6
				e.add("2026-10-10T12:00:00.000Z", 3) // 7-9
				e.clock.set("2026-10-02T00:00:00Z")
				e.seal() // August and September
				segs := e.segments()
				good, err := os.ReadFile(filepath.Join(e.dir, segs[0].FileName))
				if err != nil {
					t.Fatal(err)
				}

				d.do(t, e, segs[0])

				// Both segments are old enough to go. The damaged one is
				// the older, and nothing after it is removed either.
				e.clock.set("2026-12-20T00:00:00Z")
				r, err := e.a.Remove(ctx, 35)
				if !errors.Is(err, d.want) || r.Entries != 0 {
					t.Fatalf("Remove = %+v, %v; want %v and nothing removed", r, err, d.want)
				}
				if got := len(e.liveIDs()); got != 9 {
					t.Fatalf("%d live entries, want all 9", got)
				}

				// The full run: the same refusal, nothing sealed on top of
				// a broken archive, and a degraded health check.
				sum, err := e.a.Cycle(ctx, 35)
				if !errors.Is(err, d.want) || sum.Removed.Entries != 0 {
					t.Fatalf("Cycle = %+v, %v; want %v", sum, err, d.want)
				}
				if errors.Is(d.want, ErrInvalid) && len(sum.Sealed) != 0 {
					t.Errorf("Cycle sealed %+v although verification failed", sum.Sealed)
				}
				if got := len(e.liveIDs()); got != 9 {
					t.Fatalf("%d live entries after Cycle, want all 9", got)
				}
				if h := e.a.Health(ctx); h == "" {
					t.Error("Health reports nothing wrong")
				}

				// Put the file back: the next run removes what is due, and
				// the health check recovers.
				path := filepath.Join(e.dir, segs[0].FileName)
				os.RemoveAll(path)
				if err := os.WriteFile(path, good, 0o400); err != nil {
					t.Fatal(err)
				}
				sum, err = e.a.Cycle(ctx, 35)
				if err != nil || sum.Removed.Entries != 9 {
					t.Fatalf("Cycle after repair = %+v, %v; want all three months removed", sum, err)
				}
				if h := e.a.Health(ctx); h != "" {
					t.Errorf("Health after repair = %q", h)
				}
			})
		}
	}
}

func TestVerifyFindsTheFirstBreak(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		e.add("2026-08-10T12:00:00.000Z", 2)
		e.add("2026-09-10T12:00:00.000Z", 2)
		e.add("2026-10-10T12:00:00.000Z", 2)
		e.clock.set("2026-11-02T00:00:00Z")
		e.seal()
		segs := e.segments()
		e.verifyOK()

		// A segment moved to other storage: reported, accepted only when
		// told to.
		moved := filepath.Join(t.TempDir(), segs[0].FileName)
		if err := os.Rename(filepath.Join(e.dir, segs[0].FileName), moved); err != nil {
			t.Fatal(err)
		}
		rep, err := e.a.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := rep.Missing(); len(got) != 1 || got[0] != segs[0].FileName {
			t.Errorf("Missing = %v", got)
		}
		if err := rep.Err(false); !errors.Is(err, ErrMissing) {
			t.Errorf("Err(false) = %v, want ErrMissing", err)
		}
		if err := rep.Err(true); err != nil {
			t.Errorf("Err(true) = %v, want nil", err)
		}
		// A run tolerates it while nothing of that segment is due: this is
		// the agent's default, which never removes.
		if _, err := e.a.Cycle(ctx, 0); err != nil {
			t.Errorf("Cycle with a moved segment and no removal = %v", err)
		}
		if h := e.a.Health(ctx); h != "" {
			t.Errorf("Health = %q", h)
		}

		// Two files swapped: both are intact segments, neither is the one
		// the index names.
		a, b := filepath.Join(e.dir, segs[1].FileName), filepath.Join(e.dir, segs[2].FileName)
		tmp := a + ".swap"
		for _, mv := range [][2]string{{a, tmp}, {b, a}, {tmp, b}} {
			if err := os.Rename(mv[0], mv[1]); err != nil {
				t.Fatal(err)
			}
		}
		rep, err = e.a.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Checks[1].Status != StatusInvalid || rep.Checks[2].Status != StatusInvalid {
			t.Errorf("swapped files: %+v", rep.Checks)
		}
		if err := rep.Err(true); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "segment 2") {
			t.Errorf("Err = %v, want the first break, segment 2", err)
		}
	})
}

// TestVerifyDoesNotTrustTheIndex rewrites the index behind the triggers'
// back, as someone with write access to the database file could.
func TestVerifyDoesNotTrustTheIndex(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-09-10T12:00:00.000Z", 2)
		e.add("2026-10-10T12:00:00.000Z", 2)
		e.clock.set("2026-11-02T00:00:00Z")
		e.seal()
		if _, err := e.db.Exec(`DROP TRIGGER audit_segments_no_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Exec(`UPDATE audit_segments SET hash = ? WHERE seq = 1`, strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		rep, err := e.a.Verify(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The first segment no longer matches its file, and the second no
		// longer continues the chain.
		if rep.Checks[0].Status != StatusInvalid || rep.Checks[1].Status != StatusInvalid ||
			!strings.Contains(rep.Checks[1].Detail, "hash chain") {
			t.Errorf("checks = %+v", rep.Checks)
		}
	})
}

func TestLateSealing(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		late := func() bool {
			t.Helper()
			l, err := e.a.Late(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return l
		}
		// An empty log is never late.
		e.clock.set("2026-11-20T00:00:00Z")
		if late() {
			t.Error("an empty log is late")
		}

		e.add("2026-10-10T12:00:00.000Z", 2)
		e.add("2026-11-03T12:00:00.000Z", 1)
		// Sealing fails: the node ID is not known.
		e.a.nodeID = func(context.Context) (string, error) { return "", errors.New("agent unreachable") }

		// The 7th day of the new month is still in time; the 8th is not.
		// (Health reuses its answer for a minute, hence not 23:59:59.)
		e.clock.set("2026-11-07T23:00:00Z")
		if _, err := e.a.Cycle(ctx, 0); err == nil {
			t.Fatal("Cycle succeeded without a node ID")
		}
		if late() || e.a.Health(ctx) != "" {
			t.Errorf("late on the 7th: %v, %q", late(), e.a.Health(ctx))
		}
		e.clock.set("2026-11-07T23:59:59.999Z")
		if late() {
			t.Error("late in the last millisecond of the 7th")
		}
		e.clock.set("2026-11-08T00:00:00Z")
		if !late() || e.a.Health(ctx) == "" {
			t.Errorf("not late on the 8th: %v, %q", late(), e.a.Health(ctx))
		}
		// It stays degraded, into the next month too.
		e.clock.set("2026-12-03T00:00:00Z")
		if late() {
			// December 3rd: November is not late yet, and October's
			// entries count as November's backlog only from the 8th.
			t.Error("late on December 3rd")
		}
		e.clock.set("2026-12-09T00:00:00Z")
		if !late() {
			t.Error("not late on December 9th with October and November unsealed")
		}

		// Until the seal succeeds.
		e.restart()
		if _, err := e.a.Cycle(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if late() || e.a.Health(ctx) != "" {
			t.Errorf("still late after sealing: %v, %q", late(), e.a.Health(ctx))
		}
		if got := e.ranges(); got != "2026-10:1-2 2026-11:3-3" {
			t.Errorf("index = %q", got)
		}

		// A month without entries leaves nothing to be late with.
		e.clock.set("2027-03-20T00:00:00Z")
		if late() {
			t.Error("late although every entry is sealed")
		}
	})
}

func TestExport(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		ctx := context.Background()
		if _, err := e.db.Exec(e.kind.insert, "2026-09-10T12:00:00.000Z", "{\n  \"quote\": \"a \\\"b\\\" <c>\",\n  \"n\": [1, 2]\n}"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Exec(e.kind.insert, "2026-09-11T12:00:00.000Z", nil); err != nil {
			t.Fatal(err)
		}
		e.add("2026-10-10T12:00:00.000Z", 1)
		e.clock.set("2026-11-02T00:00:00Z")
		e.seal()

		var lines []string
		n, err := e.a.Export(ctx, "", func(line []byte) error { lines = append(lines, string(line)); return nil })
		if err != nil || n != 2 || len(lines) != 3 {
			t.Fatalf("Export = %d segments, %d lines, %v", n, len(lines), err)
		}
		// One line per entry, columns in their fixed order, NULL as null,
		// details as JSON.
		var first map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
			t.Fatalf("line is not JSON: %v\n%s", err, lines[0])
		}
		if strings.Count(lines[0], "\n") != 1 || !strings.HasPrefix(lines[0], `{"id":1,"occurred_at":"2026-09-10T12:00:00.000Z",`) {
			t.Errorf("line = %q", lines[0])
		}
		details, _ := first["details"].(map[string]any)
		if details["quote"] != `a "b" <c>` || fmt.Sprint(details["n"]) != "[1 2]" {
			t.Errorf("details = %v", first["details"])
		}
		if len(first) != len(e.kind.log.Columns) {
			t.Errorf("%d fields, want %d: every column is present", len(first), len(e.kind.log.Columns))
		}
		if !strings.HasSuffix(lines[1], `"details":null}`+"\n") {
			t.Errorf("line with NULL details = %q", lines[1])
		}

		lines = nil
		if n, err := e.a.Export(ctx, "2026-10", func(line []byte) error { lines = append(lines, string(line)); return nil }); err != nil || n != 1 || len(lines) != 1 {
			t.Fatalf("Export of October = %d segments, %d lines, %v", n, len(lines), err)
		}

		// Nothing of a file that does not match its hash is printed.
		rewrite(t, filepath.Join(e.dir, e.segments()[0].FileName), func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"andy"`), []byte(`"eve!"`), 1)
		})
		lines = nil
		if _, err := e.a.Export(ctx, "", func(line []byte) error { lines = append(lines, string(line)); return nil }); !errors.Is(err, ErrInvalid) || len(lines) != 0 {
			t.Fatalf("Export of an altered archive = %d lines, %v", len(lines), err)
		}
	})
}

func TestArchiveDirectoryRules(t *testing.T) {
	forKinds(t, func(t *testing.T, e *env) {
		e.add("2026-10-10T12:00:00.000Z", 1)
		e.clock.set("2026-11-02T00:00:00Z")

		// A directory others can enter is refused, not silently fixed.
		if err := os.Mkdir(e.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(e.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := e.a.Seal(context.Background()); err == nil || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("Seal into a 0755 directory = %v", err)
		}
		// A symbolic link is not a directory.
		os.Remove(e.dir)
		if err := os.Symlink(t.TempDir(), e.dir); err != nil {
			t.Fatal(err)
		}
		if _, err := e.a.Seal(context.Background()); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("Seal through a symbolic link = %v", err)
		}
		if len(e.segments()) != 0 {
			t.Error("a segment was recorded")
		}
		os.Remove(e.dir)
		e.seal()
		e.verifyOK()
	})
}

func TestNodeIDMustFitAFileName(t *testing.T) {
	e := newEnv(t, kinds[0])
	e.add("2026-10-10T12:00:00.000Z", 1)
	e.a.nodeID = func(context.Context) (string, error) { return "../../etc", nil }
	if _, err := e.a.Seal(context.Background()); err == nil {
		t.Fatal("a node ID with a path in it was accepted")
	}
	if len(e.files()) != 0 || len(e.segments()) != 0 {
		t.Errorf("something was written: %v", e.files())
	}
}
