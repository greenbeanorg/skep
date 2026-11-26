//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// init and init -check run as root over the service account's directory:
// a join.json that is a symbolic link or a FIFO is never followed, opened
// or waited on. Without a store it is a refusal; beside a store it is
// initialised, whatever the file holds (the service's start rule, run as
// the account, judges the phase).
func TestInitCheckDoesNotOpenJoinJSON(t *testing.T) {
	outside := t.TempDir()
	fifo := filepath.Join(outside, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte(`{"phase":"abandoning"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		make func(p string) error
	}{
		{"symbolic link to a FIFO", func(p string) error { return os.Symlink(fifo, p) }},
		{"symbolic link to a file", func(p string) error { return os.Symlink(secret, p) }},
		{"FIFO", func(p string) error { return syscall.Mkfifo(p, 0o600) }},
		{"a join in phase abandoning", func(p string) error {
			return os.WriteFile(p, []byte(`{"phase":"abandoning"}`), 0o600)
		}},
	} {
		for _, withStore := range []bool{false, true} {
			if !withStore && c.name == "a join in phase abandoning" {
				continue // covered by TestInitRefusesWhatAJoinWipeOrRotationLeft
			}
			name := c.name + map[bool]string{false: ", no store", true: ", with a store"}[withStore]
			t.Run(name, func(t *testing.T) {
				w := newWorld(t)
				if withStore {
					if err := w.init(0); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(w.dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
				os.Chmod(w.dataDir, 0o700)
				if err := c.make(filepath.Join(w.dataDir, "join.json")); err != nil {
					t.Fatal(err)
				}
				w.out.Reset()
				done := make(chan error, 1)
				go func() {
					done <- run(context.Background(), []string{"init", "-check", "-data-dir", w.dataDir, "-config", w.config}, w.env(0))
				}()
				var err error
				select {
				case err = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("init -check blocked on join.json")
				}
				if withStore {
					if err != nil || w.out.String() != "initialised\n" {
						t.Fatalf("with a store: %v %q", err, w.out.String())
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "join --abandon") || w.out.Len() != 0 {
					t.Fatalf("no store: %v %q", err, w.out.String())
				}
			})
		}
	}
}
