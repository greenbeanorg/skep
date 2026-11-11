package hardware

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSysctl serves sysctl values parsed from "name: value" fixture lines.
type fakeSysctl struct {
	values map[string]string
	// errs forces an error for specific OIDs.
	errs map[string]error
}

func loadSysctlFixture(t *testing.T, path string) *fakeSysctl {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	s := &fakeSysctl{values: map[string]string{}, errs: map[string]error{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		s.values[name] = value
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *fakeSysctl) String(name string) (string, error) {
	if err := s.errs[name]; err != nil {
		return "", err
	}
	v, ok := s.values[name]
	if !ok {
		return "", ErrSysctlNotFound
	}
	return v, nil
}

func (s *fakeSysctl) Uint64(name string) (uint64, error) {
	v, err := s.String(name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 10, 64)
}

// fakeRunner returns canned output keyed by "path arg arg ..." and records
// every command it was asked to run.
type fakeRunner struct {
	outputs map[string]string
	errs    map[string]error
	calls   []string
}

func (r *fakeRunner) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{path}, args...), " ")
	r.calls = append(r.calls, key)
	if err := r.errs[key]; err != nil {
		return nil, err
	}
	out, ok := r.outputs[key]
	if !ok {
		return nil, errors.New("fakeRunner: unexpected command: " + key)
	}
	return []byte(out), nil
}

func (r *fakeRunner) ran(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// fakeFinder maps binary names to paths.
func fakeFinder(paths map[string]string) pathFinder {
	return func(name string) (string, bool) {
		p, ok := paths[name]
		return p, ok
	}
}

// fakeExists reports the listed paths as existing.
func fakeExists(paths ...string) existsFunc {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var fixedTime = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return fixedTime }
