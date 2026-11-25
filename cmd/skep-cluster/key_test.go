package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
)

// keyService answers key/rotate, key/abort and status on a socket.
type keyService struct {
	mu     sync.Mutex
	status localapi.Status
	rotate localapi.RotateResult
	code   int
}

func (k *keyService) start(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "k.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, v any) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.code != 0 {
			w.WriteHeader(k.code)
			_, _ = w.Write([]byte(`{"error":{"code":"rotation_in_progress","message":"member x is rotating"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc(localapi.PathKeyRotate, func(w http.ResponseWriter, r *http.Request) { answer(w, k.rotate) })
	mux.HandleFunc(localapi.PathKeyAbort, func(w http.ResponseWriter, r *http.Request) { answer(w, k.rotate) })
	mux.HandleFunc(localapi.PathStatus, func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		_ = json.NewEncoder(w).Encode(k.status)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return path
}

const (
	kOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	kNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// "skep-cluster key rotate": exit 0 once the old pin is gone, 3 when the
// wait ends first (saying what it waits for), refusals passed on, --api
// refused, root only.
func TestKeyRotateCommand(t *testing.T) {
	w := newWorld(t)
	k := &keyService{rotate: localapi.RotateResult{OldPin: kOld, NewPin: kNew, OpID: "op", AddIndex: 9}}
	k.status = localapi.Status{NodeID: "n1", Members: []clusterstate.Member{{ID: "n1", Pin: kNew}}}
	sock := k.start(t)
	if err := run(context.Background(), []string{"key", "rotate", "-socket", sock}, w.env(0)); err != nil {
		t.Fatalf("rotate: %v\n%s", err, w.out.String())
	}
	out := w.out.String()
	if !strings.Contains(out, "old pin: "+kOld) || !strings.Contains(out, "new pin: "+kNew) || !strings.Contains(out, "Done:") {
		t.Fatalf("output:\n%s", out)
	}
	// The wait ends first: exit 3, naming what it waits for.
	k.status = localapi.Status{NodeID: "n1", Members: []clusterstate.Member{{ID: "n1", Pin: kOld, NextPin: kNew}},
		Rotation: &localapi.RotationStatus{Phase: "rotating"}, Rotations: []string{"the key rotation of n1 waits for star to apply its new pin (index 9)"}}
	w.out.Reset()
	var ec *exitCodeError
	err := run(context.Background(), []string{"key", "rotate", "-socket", sock, "-wait", "1s"}, w.env(0))
	if !errors.As(err, &ec) || ec.code != 3 || !strings.Contains(w.out.String(), "waits for star") {
		t.Fatalf("wait: %v\n%s", err, w.out.String())
	}
	// A refusal is passed on.
	k.code = http.StatusConflict
	if err := run(context.Background(), []string{"key", "rotate", "-socket", sock}, w.env(0)); err == nil || !strings.Contains(err.Error(), "rotation_in_progress") {
		t.Fatalf("refusal: %v", err)
	}
	k.code = 0
	// Abort: done once the next pin is gone and rotate.json too.
	k.status = localapi.Status{NodeID: "n1", Members: []clusterstate.Member{{ID: "n1", Pin: kOld}}}
	w.out.Reset()
	if err := run(context.Background(), []string{"key", "rotate", "--abort", "-socket", sock}, w.env(0)); err != nil || !strings.Contains(w.out.String(), "Aborted:") {
		t.Fatalf("abort: %v\n%s", err, w.out.String())
	}
	if err := run(context.Background(), []string{"key", "rotate", "--api"}, w.env(0)); err == nil || !strings.Contains(err.Error(), "step 8e") {
		t.Fatalf("--api: %v", err)
	}
	if err := run(context.Background(), []string{"key", "rotate", "-socket", sock}, w.env(1000)); err == nil || !strings.Contains(err.Error(), "must be run as root") {
		t.Fatalf("as a user: %v", err)
	}
	if err := run(context.Background(), []string{"key"}, w.env(0)); !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("usage: %v", err)
	}
}

// join-request, join and listen -none refuse while rotate.json exists.
func TestRefusedDuringRotation(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(w.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dataDir, layout.RotateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"join-request", "-address", "127.0.0.1", "-data-dir", w.dataDir},
		{"join", "-data-dir", w.dataDir, "x"},
		{"listen", "-none", "-data-dir", w.dataDir},
	} {
		if err := run(context.Background(), args, w.env(0)); err == nil || !strings.Contains(err.Error(), "rotating its key") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
