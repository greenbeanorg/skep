package hostops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

func rctlHost(t *testing.T, racct bool) (*host, *script) {
	t.Helper()
	h, run, _ := jailHost(t)
	sys := h.sys.(fakeSystem)
	sys.oids[sysctlNCPU] = 4
	if racct {
		sys.oids[sysctlRacct] = 1
	}
	return h, run
}

func TestJailRctlRules(t *testing.T) {
	rules, err := JailRctlRules(ctrA, JailLimits{MemoryBytes: 256 << 20, CPUPercent: 50, MaxProcesses: 100, OpenFiles: 1024})
	if err != nil {
		t.Fatal(err)
	}
	j := "jail:skep-" + ctrA + ":"
	want := []string{j + "memoryuse:deny=268435456", j + "pcpu:deny=50", j + "maxproc:deny=100", j + "openfiles:deny=1024"}
	if strings.Join(rules, " ") != strings.Join(want, " ") {
		t.Fatalf("rules = %q, want %q", rules, want)
	}
	if rules, err := JailRctlRules(ctrA, JailLimits{}); err != nil || len(rules) != 0 {
		t.Fatalf("no limits: %q, %v", rules, err)
	}
	if _, err := JailRctlRules(ctrA, JailLimits{OpenFiles: -1}); err == nil {
		t.Fatal("a negative limit was accepted")
	}
	if _, err := JailRctlRules("../x", JailLimits{CPUPercent: 1}); err == nil {
		t.Fatal("an invalid ID was accepted")
	}
}

func TestRctlInfo(t *testing.T) {
	h, _ := rctlHost(t, true)
	if info, err := h.RctlInfo(context.Background()); err != nil || !info.Enabled || info.CPUs != 4 {
		t.Fatalf("enabled: %+v, %v", info, err)
	}
	// A kernel without RACCT has no kern.racct.enable at all.
	h, _ = rctlHost(t, false)
	if info, err := h.RctlInfo(context.Background()); err != nil || info.Enabled {
		t.Fatalf("no RACCT: %+v, %v", info, err)
	}
}

func TestApplyJailRulesChecksWhatTheKernelHolds(t *testing.T) {
	h, run := rctlHost(t, true)
	spec := jailSpec()
	spec.Limits = JailLimits{MemoryBytes: 256 << 20, CPUPercent: 50}
	j := "jail:skep-" + ctrA
	run.answers["rctl "+j] = j + ":pcpu:deny=50\n" + j + ":memoryuse:deny=268435456\n"
	if err := h.applyJailRules(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{"rctl -a " + j + ":memoryuse:deny=268435456", "rctl -a " + j + ":pcpu:deny=50", "rctl " + j}
	if strings.Join(run.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", run.calls)
	}
	// A rule the kernel holds besides Skep's is a refusal.
	run.calls = nil
	run.answers["rctl "+j] = j + ":pcpu:deny=50\n" + j + ":memoryuse:deny=268435456\n" + j + ":maxproc:deny=1\n"
	if err := h.applyJailRules(context.Background(), spec); err == nil {
		t.Fatal("an extra rule was accepted")
	}
	// No limits: nothing runs.
	run.calls = nil
	if err := h.applyJailRules(context.Background(), jailSpec()); err != nil || len(run.calls) != 0 {
		t.Fatalf("no limits: %v, %q", err, run.calls)
	}
}

func TestClearJailRules(t *testing.T) {
	j := "jail:skep-" + ctrA
	// Nothing listed: rctl -r, which exits 1 when nothing matches, is not
	// run.
	h, run := rctlHost(t, true)
	if err := h.clearJailRules(context.Background(), ctrA); err != nil {
		t.Fatal(err)
	}
	if strings.Join(run.calls, "|") != "rctl "+j {
		t.Fatalf("calls = %q", run.calls)
	}
	// Rules listed: removed and checked gone.
	h, run = rctlHost(t, true)
	run.answers["rctl "+j] = []any{j + ":pcpu:deny=50\n", ""}
	if err := h.clearJailRules(context.Background(), ctrA); err != nil {
		t.Fatal(err)
	}
	if strings.Join(run.calls, "|") != "rctl "+j+"|rctl -r "+j+"|rctl "+j {
		t.Fatalf("calls = %q", run.calls)
	}
	// Rules still there after rctl -r: an error.
	h, run = rctlHost(t, true)
	run.answers["rctl "+j] = j + ":pcpu:deny=50\n"
	if err := h.clearJailRules(context.Background(), ctrA); err == nil {
		t.Fatal("rules left behind were not reported")
	}
	// Without RACCT nothing runs.
	h, run = rctlHost(t, false)
	if err := h.clearJailRules(context.Background(), ctrA); err != nil || len(run.calls) != 0 {
		t.Fatalf("no RACCT: %v, %q", err, run.calls)
	}
}

func TestStartJailRefusesLimitsWithoutRacct(t *testing.T) {
	h, run := rctlHost(t, false)
	spec := jailSpec()
	spec.Limits = JailLimits{MaxProcesses: 10}
	run.answers["jls -j skep-"+ctrA+" jid"] = &CommandError{Name: "jls", ExitCode: 1, Stderr: "jls: jail \"skep-" + ctrA + "\" not found", Err: errors.New("exit status 1")}
	err := h.checkRctlForStart(context.Background(), spec)
	wantCode(t, err, models.CodeRctlUnavailable)
	// Without limits the same host starts jails.
	if err := h.checkRctlForStart(context.Background(), jailSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestSetJailQuota(t *testing.T) {
	ds := "skeptest/skep/jail/" + ctrA
	f := make([]string, datasetFields)
	for i := range f {
		f[i] = "-"
	}
	f[0], f[1], f[17] = ds, "filesystem", ctrA
	f[4] = "1048576"
	h, run := rctlHost(t, true)
	run.answers["zfs list -H -p -t filesystem,volume -o "+datasetColumns+" -r "+ds] = strings.Join(f, "\t") + "\n"
	ctx := context.Background()
	if err := h.SetJailQuota(ctx, "skeptest", ctrA, 1<<30); err != nil {
		t.Fatal(err)
	}
	if last := run.calls[len(run.calls)-1]; last != "zfs set refquota=1073741824 "+ds {
		t.Fatalf("last call = %q", last)
	}
	if err := h.SetJailQuota(ctx, "skeptest", ctrA, 0); err != nil {
		t.Fatal(err)
	}
	if last := run.calls[len(run.calls)-1]; last != "zfs set refquota=none "+ds {
		t.Fatalf("last call = %q", last)
	}
	run.answers["zfs set refquota=67108864 "+ds] = &CommandError{Name: "zfs", ExitCode: 1,
		Stderr: "cannot set property for '" + ds + "': size is less than current used or reserved space", Err: errors.New("exit status 1")}
	wantCode(t, h.SetJailQuota(ctx, "skeptest", ctrA, 64<<20), models.CodeInvalidRequest)
	// A dataset naming another owner is not touched.
	f[17] = vmA
	run.answers["zfs list -H -p -t filesystem,volume -o "+datasetColumns+" -r "+ds] = strings.Join(f, "\t") + "\n"
	run.calls = nil
	if err := h.SetJailQuota(ctx, "skeptest", ctrA, 1<<30); err == nil {
		t.Fatal("a foreign dataset got a quota")
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs set") {
			t.Fatalf("set ran: %s", c)
		}
	}
}

func TestParseDatasetsRefQuota(t *testing.T) {
	f := make([]string, datasetFields)
	for i := range f {
		f[i] = "-"
	}
	f[0], f[1], f[20] = "skeptest/skep/jail/"+ctrA, "filesystem", "536870912"
	list, err := parseDatasets([]byte(strings.Join(f, "\t") + "\n"))
	if err != nil || len(list) != 1 || list[0].RefQuotaBytes != 512<<20 {
		t.Fatalf("parse = %+v, %v", list, err)
	}
}
