package hostops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00.00":    0,
		"0:01.50":    1500 * time.Millisecond,
		"2:03.25":    2*time.Minute + 3250*time.Millisecond,
		"125:00.00":  125 * time.Minute, // ps prints minutes past 59
		"1:02:03.00": time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00": 49 * time.Hour,
		"0:07,10":    7100 * time.Millisecond,
	} {
		got, err := parseCPUTime(in)
		if err != nil || got != want {
			t.Errorf("parseCPUTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "12", "a:00.00", "0:61.00", "1:2:3:4", "-1:00.00", "0:-1.0"} {
		if _, err := parseCPUTime(bad); err == nil {
			t.Errorf("parseCPUTime(%q) accepted", bad)
		}
	}
}

func TestParsePS(t *testing.T) {
	u, err := parsePS([]byte("  65536   0:12.34 bhyve\n"))
	if err != nil || u.ResidentBytes != 64<<20 || u.CPUTime != 12340*time.Millisecond {
		t.Fatalf("parsePS = %+v, %v", u, err)
	}
	if _, err := parsePS([]byte("100 0:00.00 sh\n")); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("a process that is not bhyve: %v", err)
	}
	for _, bad := range []string{"", "1 2", "x 0:00.00 bhyve", "-5 0:00.00 bhyve"} {
		if _, err := parsePS([]byte(bad)); err == nil {
			t.Errorf("parsePS(%q) accepted", bad)
		}
	}
}

func TestBhyveUsageChecksTheProcessIsTheVMsBhyve(t *testing.T) {
	run := &script{answers: map[string]any{
		"ps -o rss= -o time= -o comm= -p 4242": "  2048   1:00.00 bhyve\n",
		"ps -o rss= -o time= -o comm= -p 4343": &CommandError{Name: "ps", ExitCode: 1, Err: errors.New("exit status 1")},
	}}
	h := testHost(t, run, true)
	sys := h.sys.(fakeSystem)
	sys.procs[4242] = []string{"bhyve: skep-" + vmA}
	sys.procs[4343] = []string{"/usr/sbin/bhyve", "-k", "/x/bhyve.conf", "skep-" + vmA}
	sys.procs[99] = []string{"bhyve: skep-" + diskA} // another VM's bhyve
	sys.procs[98] = []string{"/bin/sh"}
	ctx := context.Background()

	u, err := h.BhyveUsage(ctx, vmA, 4242)
	if err != nil || u.ResidentBytes != 2<<20 || u.CPUTime != time.Minute {
		t.Fatalf("BhyveUsage = %+v, %v", u, err)
	}
	// Gone between the argument check and ps.
	if _, err := h.BhyveUsage(ctx, vmA, 4343); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("exited process: %v", err)
	}
	before := len(run.calls)
	for _, pid := range []int{99, 98, 7, 1, 0} {
		if _, err := h.BhyveUsage(ctx, vmA, pid); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("pid %d: %v, want not found", pid, err)
		}
	}
	if len(run.calls) != before {
		t.Errorf("ps ran for a process that is not the VM's bhyve: %v", run.calls[before:])
	}
	if _, err := h.BhyveUsage(ctx, "../etc", 4242); err == nil {
		t.Error("an invalid VM ID was accepted")
	}
}
