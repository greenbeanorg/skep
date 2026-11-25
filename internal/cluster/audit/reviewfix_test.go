package audit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func verifyOK(t *testing.T, data string) Report {
	t.Helper()
	rep, err := Verify(Dir(data))
	if err != nil || rep.Break != nil {
		t.Fatalf("verify: %v %+v", err, rep.Break)
	}
	return rep
}

// F6 of the 8b-2b review: a failed write (part of the record written) or
// a failed fsync to an existing segment leaves the file as it was: the
// next append and the next Open give a chain that verifies. When the undo
// fails too, every later append fails until the next Open, which repairs
// the tail.
func TestFailedAppendLeavesTheFileAsItWas(t *testing.T) {
	boom := errors.New("injected")
	for _, tc := range []struct {
		name string
		set  func(l *Log)
		undo bool
	}{
		{"write", func(l *Log) { l.failWrite = boom }, true},
		{"fsync", func(l *Log) { l.failSync = boom }, true},
		{"write, undo fails", func(l *Log) { l.failWrite, l.failUndo = boom, boom }, false},
		{"fsync, undo fails", func(l *Log) { l.failSync, l.failUndo = boom, boom }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			c := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
			l := mustOpen(t, data, c)
			if err := l.Started(1, 1); err != nil {
				t.Fatal(err)
			}
			if err := l.AuditEntry(entry(2, OutcomeApplied)); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(Dir(data), SegmentName(1))
			before, _ := os.ReadFile(path)
			l.mu.Lock()
			tc.set(l)
			l.mu.Unlock()
			if err := l.AuditEntry(entry(3, OutcomeApplied)); err == nil {
				t.Fatal("the failed append returned no error")
			}
			l.mu.Lock()
			l.failWrite, l.failSync, l.failUndo = nil, nil, nil
			l.mu.Unlock()
			if tc.undo {
				if after, _ := os.ReadFile(path); string(after) != string(before) {
					t.Fatalf("the file changed:\n%s", after)
				}
				if err := l.AuditEntry(entry(3, OutcomeApplied)); err != nil {
					t.Fatal(err)
				}
				verifyOK(t, data)
			} else if err := l.Event(EventListenSet, nil); err == nil || !strings.Contains(err.Error(), "an earlier write failed") {
				t.Fatalf("an append after a failed undo: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			l = mustOpen(t, data, c)
			if err := l.Event(EventListenSet, map[string]string{"address": "10.0.0.1:8444"}); err != nil {
				t.Fatal(err)
			}
			l.Close()
			rep := verifyOK(t, data)
			if tc.undo && rep.Records != 4 {
				t.Fatalf("records %d", rep.Records)
			}
		})
	}
}

// F8 of the 8b-2b review: audit verify (and ReadAll, and the tail repair
// at Open) does not follow a symbolic link or wait on a FIFO: a segment
// that is not a regular file is a break.
func TestSegmentThatIsNotARegularFile(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir()
			c := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
			l := mustOpen(t, data, c)
			if err := l.Started(1, 1); err != nil {
				t.Fatal(err)
			}
			l.Close()
			second := filepath.Join(Dir(data), SegmentName(2))
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "elsewhere")
				if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, second); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(second, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				rep, err := Verify(Dir(data))
				if err != nil || rep.Break == nil || rep.Break.File != SegmentName(2) || rep.Break.Reason != "not a regular file" {
					t.Errorf("verify: %v %+v", err, rep.Break)
				}
				if _, err := ReadAll(Dir(data)); err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Errorf("read all: %v", err)
				}
				_, err = Open(data, c.now)
				var be *BrokenError
				if !errors.As(err, &be) || be.Break.Reason != "not a regular file" {
					t.Errorf("open: %v", err)
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("blocked on the segment")
			}
		})
	}
}

// F7: Open's error for a broken audit gives the way out.
func TestBrokenAuditMessageGivesTheWayOut(t *testing.T) {
	data := t.TempDir()
	c := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	l := mustOpen(t, data, c)
	if err := l.Started(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.AuditEntry(entry(2, OutcomeApplied)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	path := filepath.Join(Dir(data), SegmentName(1))
	b, _ := os.ReadFile(path)
	b[10] ^= 1
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(data, c.now)
	for _, want := range []string{"stop skep_cluster", "skep-cluster audit verify", Dir(data) + ".broken.<UTC time>", "audit verify -dir", "audit.started"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in: %v", want, err)
		}
	}
	// Following it: the moved audit still verifies as broken; a new one
	// begins.
	moved := Dir(data) + ".broken.20261125T120000Z"
	if err := os.Rename(Dir(data), moved); err != nil {
		t.Fatal(err)
	}
	if rep, err := Verify(moved); err != nil || rep.Break == nil {
		t.Fatalf("the moved audit: %v %+v", err, rep.Break)
	}
	l = mustOpen(t, data, c)
	if !l.Empty() {
		t.Fatal("the new audit is not empty")
	}
	if err := l.Started(1, 7); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if rep := verifyOK(t, data); rep.First == nil || rep.First.Kind != KindStarted {
		t.Fatalf("first record %+v", rep.First)
	}
}
