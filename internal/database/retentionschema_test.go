package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/migrations"
)

// auditDB is one of the two databases with an audit log. The retention
// rules of ADR 0007 are the same in both, so the tests run against each.
type auditDB struct {
	name   string
	open   func(t *testing.T) *sql.DB
	insert string // one audit entry; the argument is occurred_at
}

var auditDBs = []auditDB{
	{"agent", migratedDB,
		`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome) VALUES (?, 'u1', 'andy', 1001, 'x.y', 'success')`},
	{"api", migratedAPIDB,
		`INSERT INTO audit_log (occurred_at, actor_name, action, outcome) VALUES (?, 'andy', 'auth.login', 'success')`},
}

// addEntries appends n audit entries dated in month (YYYY-MM).
func (a auditDB) addEntries(t *testing.T, db *sql.DB, month string, n int) {
	t.Helper()
	for i := range n {
		mustExec(t, db, a.insert, fmt.Sprintf("%s-10T12:00:%02d.000Z", month, i%60))
	}
}

const insertSegment = `INSERT INTO audit_segments
	(seq, month, first_id, last_id, entry_count, node_id, file_name, hash, previous_hash, sealed_at)
	VALUES (?, ?, ?, ?, ?, 'node-1', ?, ?, ?, '2026-12-01T00:00:00.000Z')`

// wantRefused runs a statement the schema must refuse, and checks that it
// was refused by a rule and not by a mistake in the statement: the error
// has to name one of wants.
func wantRefused(t *testing.T, db *sql.DB, what string, wants []string, query string, args ...any) {
	t.Helper()
	_, err := db.Exec(query, args...)
	if err == nil {
		t.Errorf("%s: statement succeeded, want it refused", what)
		return
	}
	for _, w := range wants {
		if strings.Contains(err.Error(), w) {
			return
		}
	}
	t.Errorf("%s: refused with %q, want one of %q", what, err, wants)
}

var (
	notArchived   = []string{"audit_log: an entry may be removed only after it is archived"}
	appendOnly    = []string{"audit_log is append-only"}
	intoSegment   = []string{"audit_log: an entry cannot be added inside a sealed segment"}
	segmentRule   = []string{"audit_segments: ", "constraint failed"}
	segmentFrozen = []string{"audit_segments is insert-only"}
)

func segHash(c string) string { return strings.Repeat(c, 64) }

func auditCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAuditDeleteNeedsSealedSegment checks decision 4: the database refuses
// every delete of an audit entry that is not covered by a sealed segment,
// and allows exactly the covered ones.
func TestAuditDeleteNeedsSealedSegment(t *testing.T) {
	for _, a := range auditDBs {
		t.Run(a.name, func(t *testing.T) {
			db := a.open(t)
			a.addEntries(t, db, "2026-09", 4) // IDs 1-4
			a.addEntries(t, db, "2026-10", 4) // IDs 5-8

			// Nothing is sealed: no delete of any shape is accepted.
			for name, q := range map[string]string{
				"one entry":   `DELETE FROM audit_log WHERE id = 1`,
				"the newest":  `DELETE FROM audit_log WHERE id = 8`,
				"a range":     `DELETE FROM audit_log WHERE id BETWEEN 2 AND 6`,
				"by time":     `DELETE FROM audit_log WHERE occurred_at < '2026-10-01'`,
				"everything":  `DELETE FROM audit_log`,
				"by subquery": `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log LIMIT 1)`,
			} {
				wantRefused(t, db, "unsealed: "+name, notArchived, q)
			}
			if n := auditCount(t, db); n != 8 {
				t.Fatalf("%d entries left, want 8", n)
			}

			mustExec(t, db, insertSegment, 1, "2026-09", 1, 4, 4, "seg-1", segHash("a"), "")

			// Deletes that reach outside the segment are refused as a whole:
			// the statement is atomic, so the covered rows stay too.
			for name, q := range map[string]string{
				"first uncovered entry":   `DELETE FROM audit_log WHERE id = 5`,
				"range across the border": `DELETE FROM audit_log WHERE id BETWEEN 3 AND 5`,
				"everything":              `DELETE FROM audit_log`,
			} {
				wantRefused(t, db, "one segment: "+name, notArchived, q)
			}
			if n := auditCount(t, db); n != 8 {
				t.Fatalf("%d entries left after refused deletes, want 8", n)
			}

			// An archived entry is still never modified.
			wantRefused(t, db, "update of an archived entry", appendOnly, `UPDATE audit_log SET action = 'edited' WHERE id = 1`)

			// Covered entries can go, one by one or as the whole segment.
			mustExec(t, db, `DELETE FROM audit_log WHERE id = 2`)
			mustExec(t, db, `DELETE FROM audit_log WHERE id BETWEEN 1 AND 4`)
			if n := auditCount(t, db); n != 4 {
				t.Fatalf("%d entries left, want the 4 unsealed ones", n)
			}

			// Nothing can be put back inside the sealed range, where it would
			// count as archived without being in any file.
			explicit := strings.Replace(strings.Replace(a.insert, "(occurred_at,", "(id, occurred_at,", 1), "VALUES (", "VALUES (3, ", 1)
			wantRefused(t, db, "insert with an archived ID", intoSegment, explicit, "2026-09-10T12:00:00.000Z")
			if n := auditCount(t, db); n != 4 {
				t.Fatalf("%d entries after the refused insert, want 4", n)
			}

			// New entries are unaffected and stay protected.
			a.addEntries(t, db, "2026-11", 1) // ID 9
			wantRefused(t, db, "new entry", notArchived, `DELETE FROM audit_log WHERE id = 9`)
		})
	}
}

// TestAuditSegmentRules checks that the index of sealed segments is
// insert-only and that the database refuses a segment that overlaps
// another, leaves a gap, breaks the chain, or does not match the log.
func TestAuditSegmentRules(t *testing.T) {
	for _, a := range auditDBs {
		t.Run(a.name, func(t *testing.T) {
			db := a.open(t)
			a.addEntries(t, db, "2026-09", 4) // IDs 1-4
			a.addEntries(t, db, "2026-10", 4) // IDs 5-8
			a.addEntries(t, db, "2026-11", 2) // IDs 9-10

			type seg struct {
				seq         int
				month       string
				first, last int
				count       int
				file, hash  string
				prev        string
			}
			refuse := func(name string, s seg) {
				t.Helper()
				wantRefused(t, db, name, segmentRule, insertSegment, s.seq, s.month, s.first, s.last, s.count, s.file, s.hash, s.prev)
			}
			first := seg{1, "2026-09", 1, 4, 4, "seg-1", segHash("a"), ""}

			mod := func(s seg, f func(*seg)) seg { f(&s); return s }
			refuse("first segment that skips the first entry", mod(first, func(s *seg) { s.first, s.count = 2, 3 }))
			refuse("first segment with a predecessor hash", mod(first, func(s *seg) { s.prev = segHash("f") }))
			refuse("first segment that is not number 1", mod(first, func(s *seg) { s.seq = 2 }))
			refuse("segment with the wrong count", mod(first, func(s *seg) { s.count = 3 }))
			refuse("segment that ends beyond the log", mod(first, func(s *seg) { s.last, s.count = 99, 10 }))
			refuse("segment with last before first", mod(first, func(s *seg) { s.first, s.last, s.count = 4, 1, 1 }))
			refuse("segment with a path as file name", mod(first, func(s *seg) { s.file = "../seg-1" }))
			refuse("segment with a malformed month", mod(first, func(s *seg) { s.month = "2026-9" }))
			refuse("segment with a short hash", mod(first, func(s *seg) { s.hash = "abc" }))
			refuse("segment without entries", mod(first, func(s *seg) { s.count = 0 }))

			mustExec(t, db, insertSegment, first.seq, first.month, first.first, first.last, first.count, first.file, first.hash, first.prev)

			second := seg{2, "2026-10", 5, 8, 4, "seg-2", segHash("b"), segHash("a")}
			refuse("overlapping segment", mod(second, func(s *seg) { s.first, s.count = 4, 5 }))
			refuse("segment inside another", mod(second, func(s *seg) { s.first, s.last, s.count = 2, 3, 2 }))
			refuse("segment that leaves a gap", mod(second, func(s *seg) { s.first, s.count = 6, 3 }))
			refuse("segment for the same month", mod(second, func(s *seg) { s.month = "2026-09" }))
			refuse("segment for an earlier month", mod(second, func(s *seg) { s.month = "2026-08" }))
			refuse("segment that breaks the hash chain", mod(second, func(s *seg) { s.prev = segHash("c") }))
			refuse("segment without a predecessor hash", mod(second, func(s *seg) { s.prev = "" }))
			refuse("segment that skips a number", mod(second, func(s *seg) { s.seq = 3 }))
			refuse("segment that reuses a number", mod(second, func(s *seg) { s.seq = 1 }))
			refuse("segment that reuses a file name", mod(second, func(s *seg) { s.file = "seg-1" }))

			mustExec(t, db, insertSegment, second.seq, second.month, second.first, second.last, second.count, second.file, second.hash, second.prev)

			// The index is never modified or shortened.
			wantRefused(t, db, "segment update", segmentFrozen, `UPDATE audit_segments SET last_id = 10 WHERE seq = 2`)
			wantRefused(t, db, "segment hash update", segmentFrozen, `UPDATE audit_segments SET hash = ? WHERE seq = 1`, segHash("e"))
			wantRefused(t, db, "segment delete", segmentFrozen, `DELETE FROM audit_segments WHERE seq = 2`)
			wantRefused(t, db, "delete of all segments", segmentFrozen, `DELETE FROM audit_segments`)

			// A segment can follow one whose entries were already removed.
			mustExec(t, db, `DELETE FROM audit_log WHERE id BETWEEN 1 AND 8`)
			mustExec(t, db, insertSegment, 3, "2026-11", 9, 10, 2, "seg-3", segHash("c"), segHash("b"))
			wantRefused(t, db, "removing a segment whose entries are gone", segmentFrozen, `DELETE FROM audit_segments WHERE seq = 1`)
		})
	}
}

// TestTaskDeleteRules checks decision 1: an unfinished task is never
// deleted, and events leave only together with their task.
func TestTaskDeleteRules(t *testing.T) {
	db := migratedDB(t)
	const event = `INSERT INTO task_events (task_id, message) VALUES (?, 'progress')`

	for _, id := range []string{"pending", "running", "done"} {
		mustExec(t, db, insertTask, id, nil, nil)
		mustExec(t, db, event, id)
		mustExec(t, db, event, id)
	}
	mustExec(t, db, `UPDATE tasks SET status = 'running' WHERE id IN ('running', 'done')`)
	mustExec(t, db, `UPDATE tasks SET status = 'succeeded', finished_at = '2026-01-01T00:00:00.000Z' WHERE id = 'done'`)

	unfinished := []string{"tasks: an unfinished task cannot be deleted"}
	withTask := []string{"task_events: events are removed only together with their task"}
	wantRefused(t, db, "delete of a pending task", unfinished, `DELETE FROM tasks WHERE id = 'pending'`)
	wantRefused(t, db, "delete of a running task", unfinished, `DELETE FROM tasks WHERE id = 'running'`)
	wantRefused(t, db, "delete of all tasks", unfinished, `DELETE FROM tasks`)
	for _, id := range []string{"pending", "running", "done"} {
		wantRefused(t, db, "delete of the events of task "+id, withTask, `DELETE FROM task_events WHERE task_id = ?`, id)
	}
	wantRefused(t, db, "delete of all events", withTask, `DELETE FROM task_events`)

	var tasks, events int
	count := func() {
		t.Helper()
		if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM tasks), (SELECT COUNT(*) FROM task_events)`).Scan(&tasks, &events); err != nil {
			t.Fatal(err)
		}
	}
	if count(); tasks != 3 || events != 6 {
		t.Fatalf("after refused deletes: %d tasks, %d events; want 3 and 6", tasks, events)
	}

	// A finished task goes, and its events with it.
	mustExec(t, db, `DELETE FROM tasks WHERE id = 'done'`)
	if count(); tasks != 2 || events != 4 {
		t.Fatalf("after deleting the finished task: %d tasks, %d events; want 2 and 4", tasks, events)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = 'done'`).Scan(&left); err != nil || left != 0 {
		t.Errorf("events of the deleted task left: %d, %v", left, err)
	}
}

// TestRetentionMigrationOnExistingData upgrades a database that already
// holds tasks and audit entries, as an installed node will: nothing is
// lost, and the entries are protected by the new rule from the first
// moment.
func TestRetentionMigrationOnExistingData(t *testing.T) {
	for _, a := range auditDBs {
		t.Run(a.name, func(t *testing.T) {
			ctx := context.Background()
			all := migrations.Agent()
			if a.name == "api" {
				all = migrations.API()
			}
			known, err := LoadMigrations(all)
			if err != nil {
				t.Fatal(err)
			}
			before := fstest.MapFS{}
			for _, m := range known[:2] {
				name := fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)
				body, err := fs.ReadFile(all, name)
				if err != nil {
					t.Fatal(err)
				}
				before[name] = &fstest.MapFile{Data: body}
			}

			db := openTestDB(t)
			if _, err := Migrate(ctx, db, before); err != nil {
				t.Fatal(err)
			}
			a.addEntries(t, db, "2026-10", 3)
			wantRefused(t, db, "delete before the upgrade", appendOnly, `DELETE FROM audit_log`)

			applied, err := Migrate(ctx, db, all)
			if err != nil {
				t.Fatal(err)
			}
			// Later migrations follow; retention is the first one applied.
			if len(applied) == 0 || applied[0].Name != "retention" {
				t.Fatalf("applied = %+v", applied)
			}
			if n := auditCount(t, db); n != 3 {
				t.Fatalf("%d entries after the upgrade, want 3", n)
			}
			wantRefused(t, db, "delete after the upgrade", notArchived, `DELETE FROM audit_log`)
			wantRefused(t, db, "update after the upgrade", appendOnly, `UPDATE audit_log SET action = 'edited'`)
		})
	}
}
