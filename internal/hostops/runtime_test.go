package hostops

import (
	"context"
	"errors"
	"os"
	"reflect"
	"syscall"
	"testing"
)

const vmB = "1c1c6f4e-5d0c-4a52-9a43-0d2f8a1f3c22"

func ifconfigMissing(name string) error {
	return &CommandError{Name: "ifconfig", ExitCode: 1, Stderr: "ifconfig: interface " + name + " does not exist", Err: errors.New("exit status 1")}
}

func TestCreateTapDescribesAndBringsUp(t *testing.T) {
	run := &script{answers: map[string]any{"ifconfig tap create": "tap3\n"}}
	h := testHost(t, run, true)
	tap, err := h.CreateTap(context.Background(), vmA, 1)
	if err != nil || tap != "tap3" {
		t.Fatalf("CreateTap = %q, %v", tap, err)
	}
	want := []string{"ifconfig tap create", "ifconfig tap3 description skep:" + vmA + ":1 up"}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("calls = %q", run.calls)
	}

	// A name that is not a tap is never used, and a failed setup destroys
	// the tap it made.
	run = &script{answers: map[string]any{"ifconfig tap create": "vtnet0\n"}}
	if _, err := testHost(t, run, true).CreateTap(context.Background(), vmA, 0); err == nil {
		t.Error("an unexpected interface name was accepted")
	}
	run = &script{answers: map[string]any{
		"ifconfig tap create":                             "tap4\n",
		"ifconfig tap4 description skep:" + vmA + ":0 up": errors.New("boom"),
	}}
	if _, err := testHost(t, run, true).CreateTap(context.Background(), vmA, 0); err == nil {
		t.Error("a failed setup was not reported")
	}
	if run.calls[len(run.calls)-1] != "ifconfig tap4 destroy" {
		t.Errorf("calls = %q, want the tap destroyed", run.calls)
	}
	if _, err := h.CreateTap(context.Background(), "../x", 0); err == nil {
		t.Error("an invalid VM ID was accepted")
	}
}

func TestAddBridgeMemberRefusesManagementAndMissingBridges(t *testing.T) {
	ok := "bridge0: flags=1008843<UP,BROADCAST,RUNNING> metric 0 mtu 1500\n\tmember: vtnet1 flags=143<LEARNING>\n"
	run := &script{answers: map[string]any{"ifconfig bridge0": ok}}
	h := testHost(t, run, true)
	if err := h.AddBridgeMember(context.Background(), "bridge0", "tap0"); err != nil {
		t.Fatal(err)
	}
	if run.calls[len(run.calls)-1] != "ifconfig bridge0 addm tap0" {
		t.Errorf("calls = %q", run.calls)
	}

	mgmt := "bridge0: flags=1008843<UP> metric 0 mtu 1500\n\tmember: vtnet0 flags=143<LEARNING>\n"
	run = &script{answers: map[string]any{"ifconfig bridge0": mgmt}}
	wantCode(t, testHost(t, run, true).AddBridgeMember(context.Background(), "bridge0", "tap0"), "bridge_not_allowed")
	run = &script{answers: map[string]any{"ifconfig bridge0": ifconfigMissing("bridge0")}}
	wantCode(t, testHost(t, run, true).AddBridgeMember(context.Background(), "bridge0", "tap0"), "bridge_not_allowed")
	wantCode(t, h.AddBridgeMember(context.Background(), "bridge9", "tap0"), "bridge_not_allowed")
	if err := h.AddBridgeMember(context.Background(), "bridge0", "vtnet0"); err == nil {
		t.Error("a non-tap member was accepted")
	}
	for _, c := range run.calls {
		if c == "ifconfig bridge0 addm tap0" {
			t.Error("a refused bridge got a member")
		}
	}
}

func TestDestroyTapOnlyDestroysTheVMsOwn(t *testing.T) {
	desc := func(id string) string {
		return "tap0: flags=8943<UP> metric 0 mtu 1500\n\tdescription: skep:" + id + ":0\n"
	}
	run := &script{answers: map[string]any{"ifconfig tap0": desc(vmB)}}
	h := testHost(t, run, true)
	if err := h.DestroyTap(context.Background(), vmA, "tap0"); err == nil {
		t.Fatal("another VM's tap was destroyed")
	}
	run.answers["ifconfig tap0"] = desc(vmA)
	if err := h.DestroyTap(context.Background(), vmA, "tap0"); err != nil {
		t.Fatal(err)
	}
	if run.calls[len(run.calls)-1] != "ifconfig tap0 destroy" {
		t.Errorf("calls = %q", run.calls)
	}
	run.answers["ifconfig tap0"] = ifconfigMissing("tap0")
	if err := h.DestroyTap(context.Background(), vmA, "tap0"); err != nil {
		t.Errorf("a missing tap: %v", err)
	}
}

func TestVMTapsMatchesDescriptions(t *testing.T) {
	run := &script{answers: map[string]any{
		"ifconfig -g tap": "tap0\ntap1\ntap2\n",
		"ifconfig tap0":   "tap0: flags=0<>\n\tdescription: skep:" + vmA + ":0\n",
		"ifconfig tap1":   "tap1: flags=0<>\n\tdescription: skep:" + vmB + ":0\n",
		"ifconfig tap2":   "tap2: flags=0<>\n",
	}}
	taps, err := testHost(t, run, true).VMTaps(context.Background(), vmA)
	if err != nil || !reflect.DeepEqual(taps, []string{"tap0"}) {
		t.Fatalf("VMTaps = %v, %v", taps, err)
	}
}

func TestInstanceControlUsesBhyvectl(t *testing.T) {
	run := &script{answers: map[string]any{}}
	h := testHost(t, run, true)
	if err := h.DestroyInstance(context.Background(), vmA); err != nil || len(run.calls) != 0 {
		t.Fatalf("destroying a missing instance ran %q, %v", run.calls, err)
	}
	h.sys.(fakeSystem).paths["/dev/vmm/skep-"+vmA] = true
	if err := h.ForcePoweroff(context.Background(), vmA); err != nil {
		t.Fatal(err)
	}
	if err := h.DestroyInstance(context.Background(), vmA); err != nil {
		t.Fatal(err)
	}
	want := []string{"bhyvectl --vm=skep-" + vmA + " --force-poweroff", "bhyvectl --vm=skep-" + vmA + " --destroy"}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("calls = %q", run.calls)
	}
}

// fakeSpawner records what it starts.
type fakeSpawner struct {
	started [][]string
	opts    []spawnOptions
	pid     int
}

type nopProcess struct{ pid int }

func (p nopProcess) PID() int                  { return p.pid }
func (nopProcess) Wait() ExitStatus            { return ExitStatus{} }
func (nopProcess) Signal(syscall.Signal) error { return nil }

func (s *fakeSpawner) Spawn(path string, args []string, opts spawnOptions) (Process, error) {
	s.started = append(s.started, append([]string{path}, args...))
	s.opts = append(s.opts, opts)
	return nopProcess{pid: s.pid}, nil
}

func TestRunnerIsStartedDetachedAndRecognisedByItsArguments(t *testing.T) {
	h := testHost(t, &script{}, true)
	sp := &fakeSpawner{pid: 4242}
	h.spawn, h.self = sp, "/usr/local/sbin/skep-agent"
	sys := h.sys.(fakeSystem)

	pid, err := h.StartRunner(context.Background(), vmA)
	if err != nil || pid != 4242 {
		t.Fatalf("StartRunner = %d, %v", pid, err)
	}
	want := []string{"/usr/local/sbin/skep-agent", "vm-runner", "-vm", vmA, "-run-dir", h.conf.RunDir}
	if !reflect.DeepEqual(sp.started[0], want) || !sp.opts[0].NewSession {
		t.Fatalf("started %q with %+v", sp.started[0], sp.opts[0])
	}
	raw, err := h.ReadRunFile(vmA, RunnerPIDFile)
	if err != nil || string(raw) != "4242\n" {
		t.Fatalf("runner.pid = %q, %v", raw, err)
	}
	if fi, err := os.Stat(h.conf.RunDir + "/" + vmA); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir: %v %v", fi, err)
	}

	// Not running: not a runner.
	if _, ok := h.Runner(vmA); ok {
		t.Error("a dead process was taken for a runner")
	}
	// A recycled PID running something else is never a runner, and is
	// never signalled.
	sys.procs[4242] = []string{"/bin/sh", "-c", "sleep"}
	if _, ok := h.Runner(vmA); ok {
		t.Error("a recycled PID was taken for a runner")
	}
	if err := h.SignalRunner(vmA, StopGraceful); err == nil || len(*sys.kills) != 0 {
		t.Errorf("signalled a process that is not the runner: %v %v", err, *sys.kills)
	}
	// Another VM's runner is not this VM's.
	sys.procs[4242] = []string{"skep-agent", "vm-runner", "-vm", vmB, "-run-dir", h.conf.RunDir}
	if _, ok := h.Runner(vmA); ok {
		t.Error("another VM's runner was taken for this one's")
	}
	sys.procs[4242] = append([]string{"skep-agent"}, h.runnerArgs(vmA)...)
	if p, ok := h.Runner(vmA); !ok || p != 4242 {
		t.Fatalf("Runner = %d, %v", p, ok)
	}
	if _, err := h.StartRunner(context.Background(), vmA); err == nil {
		t.Error("a second runner was started")
	}
	if err := h.SignalRunner(vmA, StopGraceful); err != nil {
		t.Fatal(err)
	}
	if err := h.SignalRunner(vmA, StopForce); err != nil {
		t.Fatal(err)
	}
	want = []string{"4242 " + syscall.SIGTERM.String(), "4242 " + syscall.SIGUSR1.String()}
	if !reflect.DeepEqual(*sys.kills, want) {
		t.Errorf("kills = %q", *sys.kills)
	}
}

func TestRunFilesAreFixedNames(t *testing.T) {
	h := testHost(t, &script{}, true)
	if err := h.PrepareRun(context.Background(), vmA); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteRunFile(vmA, "../../etc/passwd", nil); err == nil {
		t.Error("an arbitrary run file name was accepted")
	}
	if _, err := h.StateFilePath(vmA, "x.conf"); err == nil {
		t.Error("an arbitrary state file name was accepted")
	}
	st := RunState{State: "running", RunnerPID: 7, Taps: map[int]string{0: "tap0"}, Boots: 1}
	if err := WriteRunState(h, vmA, st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadRunState(h, vmA)
	if err != nil || !ok || got.State != "running" || got.Taps[0] != "tap0" {
		t.Fatalf("ReadRunState = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := ReadRunState(h, vmB); ok {
		t.Error("a VM without a run directory has a run state")
	}
	ids, err := h.RunVMs()
	if err != nil || !reflect.DeepEqual(ids, []string{vmA}) {
		t.Errorf("RunVMs = %v, %v", ids, err)
	}
}

func TestWriteAtomicReplacesAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	for _, content := range []string{"first", "second"} {
		if err := writeAtomic(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != content {
			t.Fatalf("content = %q, %v", raw, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if err := writeAtomic(dir+"/missing/x", nil); err == nil {
		t.Error("a write into a missing directory succeeded")
	}
}
