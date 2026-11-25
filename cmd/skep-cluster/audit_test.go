package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
)

// "skep-cluster audit verify": root only, read-only, the agent's exit
// codes, and a changed byte named by file and line.
func TestAuditVerifyCommand(t *testing.T) {
	w := newWorld(t)
	if err := w.init(0, "-address", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// init wrote node.init.
	err := run(context.Background(), []string{"audit", "verify", "-data-dir", w.dataDir}, w.env(0))
	if err != nil || !strings.Contains(w.out.String(), "Records:         1 in 1 segment(s)") || !strings.Contains(w.out.String(), "node.init") {
		t.Fatalf("verify: %v\n%s", err, w.out.String())
	}
	var ec *exitCodeError
	if err := run(context.Background(), []string{"audit", "verify", "-data-dir", w.dataDir}, w.env(1000)); err == nil || !strings.Contains(err.Error(), "must be run as root") {
		t.Fatalf("as a user: %v", err)
	}
	if err := run(context.Background(), []string{"audit"}, w.env(0)); !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("usage: %v", err)
	}
	if err := run(context.Background(), []string{"audit", "verify", "extra"}, w.env(0)); !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("usage: %v", err)
	}
	// A copy with one byte changed.
	cp := t.TempDir()
	seg := audit.SegmentName(1)
	b, err := os.ReadFile(filepath.Join(audit.Dir(w.dataDir), seg))
	if err != nil {
		t.Fatal(err)
	}
	b[20] ^= 0x01
	if err := os.WriteFile(filepath.Join(cp, seg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(cp)
	w.out.Reset()
	err = run(context.Background(), []string{"audit", "verify", "-dir", cp}, w.env(0))
	if !errors.As(err, &ec) || ec.code != 1 || !strings.Contains(w.out.String(), "BROKEN:          "+seg+" line 1:") {
		t.Fatalf("a changed byte: %v\n%s", err, w.out.String())
	}
	after, _ := os.ReadDir(cp)
	if len(after) != len(before) {
		t.Fatal("verify created a file")
	}
	// A missing directory is an empty audit, and is not created.
	missing := filepath.Join(t.TempDir(), "none")
	w.out.Reset()
	if err := run(context.Background(), []string{"audit", "verify", "-dir", missing}, w.env(0)); err != nil || !strings.Contains(w.out.String(), "empty") {
		t.Fatalf("missing: %v %s", err, w.out.String())
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verify created the directory")
	}
}
