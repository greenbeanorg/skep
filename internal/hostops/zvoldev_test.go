package hostops

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// ctxScript is a script that refuses to run once its context has ended,
// as exec.CommandContext does, and records each call's context error.
type ctxScript struct {
	*script
	ctxErrs []error
}

func (c *ctxScript) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	if err := ctx.Err(); err != nil {
		c.calls = append(c.calls, "refused: "+filepath.Base(path)+" "+strings.Join(args, " "))
		return nil, err
	}
	return c.script.Run(ctx, path, args...)
}

const devDS = "skeptest/skep/vm/" + vmA + "/disk1"

var (
	devPath  = "/dev/zvol/" + devDS
	devList  = "zfs list -H -p -t filesystem,volume -o " + listCols + " " + devDS
	setNone  = "zfs set volmode=none " + devDS
	setDev   = "zfs set volmode=dev " + devDS
	devSpec  = VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1}
	charMode = fs.ModeDevice | fs.ModeCharDevice | 0o640
)

func stoppedOK(context.Context) error { return nil }

// devHost is a host whose device waits are short, with the disk's zvol
// listed as line.
func devHost(t *testing.T, line string) (*host, *script, fakeSystem) {
	t.Helper()
	run := &script{answers: map[string]any{devList: line}}
	h := testHost(t, run, true)
	h.dev = deviceTimings{late: 20 * time.Millisecond, settle: 20 * time.Millisecond, poll: time.Millisecond,
		backoff: time.Millisecond, attempts: 3}
	return h, run, h.sys.(fakeSystem)
}

func volLine(kv ...string) string {
	base := map[string]string{"volsize": "8589934592", "volmode": "dev", "volblocksize": "16384", "createtxg": "12",
		"encryption": "off", "skep:vm_id": vmA, "skep:disk_id": diskA1}
	for i := 0; i+1 < len(kv); i += 2 {
		base[kv[i]] = kv[i+1]
	}
	var args []string
	for k, v := range base {
		args = append(args, k, v)
	}
	return zline(devDS, "volume", args...)
}

// sets returns the zfs set commands among calls.
func sets(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "zfs set ") || strings.HasPrefix(c, "refused: zfs set ") {
			out = append(out, c)
		}
	}
	return out
}

// onlyNoneAndDev fails the test if any zfs set writes something other
// than volmode=none or volmode=dev of the disk.
func onlyNoneAndDev(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range sets(calls) {
		c = strings.TrimPrefix(c, "refused: ")
		if c != setNone && c != setDev {
			t.Errorf("wrote something other than volmode none or dev: %q", c)
		}
	}
}

func wantReason(t *testing.T, err error, code, reason string) *models.Error {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	if reason != "" && me.Details["reason"] != reason {
		t.Fatalf("reason = %v, want %s (%v)", me.Details["reason"], reason, err)
	}
	return me
}

func TestEnsureVolumeDevicePresentRunsNothing(t *testing.T) {
	h, run, sys := devHost(t, volLine())
	sys.modes[devPath] = charMode
	res, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK)
	if err != nil || res.Rounds != 0 || res.Path != devPath {
		t.Fatalf("= %+v, %v", res, err)
	}
	if len(run.calls) != 0 {
		t.Errorf("commands ran for a present node: %v", run.calls)
	}
}

func TestEnsureVolumeDeviceWaitsForALateNode(t *testing.T) {
	h, run, sys := devHost(t, volLine())
	h.dev.late = 5 * time.Second
	looks := 0
	sys.lstat = func(string) (fs.FileMode, error) {
		if looks++; looks < 5 {
			return 0, fs.ErrNotExist
		}
		return charMode, nil
	}
	h.sys = sys
	res, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK)
	if err != nil || res.Rounds != 0 {
		t.Fatalf("= %+v, %v", res, err)
	}
	if len(run.calls) != 0 {
		t.Errorf("a late node was repaired: %v", run.calls)
	}
}

func TestEnsureVolumeDeviceRefusals(t *testing.T) {
	ctx := context.Background()
	running := func(context.Context) error {
		return &models.Error{Code: models.CodeVMNotStopped, Message: "running"}
	}
	for name, tc := range map[string]struct {
		line    any
		spec    VolumeSpec
		stopped StoppedCheck
		setup   func(sys fakeSystem)
		code    string
		reason  string
		check   func(t *testing.T, me *models.Error)
	}{
		"not a character device": {line: volLine(), spec: devSpec, stopped: stoppedOK,
			setup: func(sys fakeSystem) { sys.modes[devPath] = 0o644 }},
		"no stopped check": {line: volLine(), spec: devSpec, code: models.CodeDiskDeviceMissing, reason: "no_stopped_check"},
		"pool not allowed": {line: volLine(), spec: VolumeSpec{Pool: "zroot", VMID: vmA, DiskID: diskA1, Slot: 1},
			stopped: stoppedOK, code: models.CodePoolNotAllowed},
		"invalid VM ID": {line: volLine(), spec: VolumeSpec{Pool: "skeptest", VMID: "../x", DiskID: diskA1, Slot: 1},
			stopped: stoppedOK, code: models.CodeInvalidRequest},
		"no such dataset":       {line: noSuchDataset(), spec: devSpec, stopped: stoppedOK},
		"not a volume":          {line: zline(devDS, "filesystem", "skep:vm_id", vmA, "skep:disk_id", diskA1), spec: devSpec, stopped: stoppedOK},
		"another disk's volume": {line: volLine("skep:disk_id", diskA), spec: devSpec, stopped: stoppedOK},
		"another VM's volume":   {line: volLine("skep:vm_id", vmB), spec: devSpec, stopped: stoppedOK},
		"key unavailable": {line: volLine("encryption", "aes-256-gcm", "keystatus", "unavailable"), spec: devSpec,
			stopped: stoppedOK, code: models.CodeStorageLocked},
		"key status unknown": {line: volLine("encryption", "aes-256-gcm", "keystatus", "unknown"), spec: devSpec,
			stopped: stoppedOK, code: models.CodeStorageLocked},
		"volmode none": {line: volLine("volmode", "none"), spec: devSpec, stopped: stoppedOK,
			code: models.CodeDiskDeviceMissing, reason: "volmode_none",
			check: func(t *testing.T, me *models.Error) {
				if me.Details["command"] != "zfs set volmode=dev "+devDS || me.Details["dataset"] != devDS {
					t.Errorf("details = %v", me.Details)
				}
				if !strings.Contains(me.Message, "zfs set volmode=dev "+devDS) {
					t.Errorf("message lacks the command: %q", me.Message)
				}
			}},
		"volmode geom": {line: volLine("volmode", "geom"), spec: devSpec, stopped: stoppedOK,
			code: models.CodeDiskDeviceMissing, reason: "volmode_not_dev"},
		"volmode default": {line: volLine("volmode", "default"), spec: devSpec, stopped: stoppedOK,
			code: models.CodeDiskDeviceMissing, reason: "volmode_not_dev"},
		"VM running": {line: volLine(), spec: devSpec, stopped: running,
			code: models.CodeDiskDeviceMissing, reason: "vm_not_stopped"},
		"stopped check fails": {line: volLine(), spec: devSpec, stopped: func(context.Context) error { return errors.New("inspect failed") },
			code: models.CodeDiskDeviceMissing, reason: "vm_not_stopped"},
		"vmm instance exists": {line: volLine(), spec: devSpec, stopped: stoppedOK,
			setup: func(sys fakeSystem) { sys.paths[filepath.Join(vmmDeviceDir, InstancePrefix+vmA)] = true },
			code:  models.CodeDiskDeviceMissing, reason: "vm_not_stopped"},
	} {
		t.Run(name, func(t *testing.T) {
			h, run, sys := devHost(t, "")
			run.answers[devList] = tc.line
			if tc.setup != nil {
				tc.setup(sys)
			}
			_, err := h.EnsureVolumeDevice(ctx, tc.spec, tc.stopped)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.code != "" {
				me := wantReason(t, err, tc.code, tc.reason)
				if tc.check != nil {
					tc.check(t, me)
				}
			}
			if s := sets(run.calls); len(s) != 0 {
				t.Errorf("a refusal changed the zvol: %v", s)
			}
		})
	}
}

func TestEnsureVolumeDeviceRepairs(t *testing.T) {
	h, run, sys := devHost(t, volLine())
	run.onRun = func(call string) {
		if call == setDev {
			sys.modes[devPath] = charMode
		}
	}
	res, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK)
	if err != nil || res.Rounds != 1 {
		t.Fatalf("= %+v, %v", res, err)
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev}) {
		t.Errorf("sets = %v", s)
	}
}

func TestEnsureVolumeDeviceSecondRound(t *testing.T) {
	h, run, sys := devHost(t, volLine())
	devs := 0
	run.onRun = func(call string) {
		if call == setDev {
			if devs++; devs == 2 {
				sys.modes[devPath] = charMode
			}
		}
	}
	res, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK)
	if err != nil || res.Rounds != 2 {
		t.Fatalf("= %+v, %v", res, err)
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev, setNone, setDev}) {
		t.Errorf("sets = %v", s)
	}
}

func TestEnsureVolumeDeviceStopsAfterTwoRounds(t *testing.T) {
	h, run, _ := devHost(t, volLine())
	checks := 0
	stopped := func(context.Context) error { checks++; return nil }
	res, err := h.EnsureVolumeDevice(context.Background(), devSpec, stopped)
	me := wantReason(t, err, models.CodeDiskDeviceMissing, "repair_failed")
	if res.Rounds != maxDeviceRepairRounds || me.Details["rounds"] != maxDeviceRepairRounds {
		t.Errorf("rounds = %d, details %v", res.Rounds, me.Details)
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev, setNone, setDev}) {
		t.Errorf("sets = %v", s)
	}
	if checks != maxDeviceRepairRounds {
		t.Errorf("the stopped check ran %d times, want once per round", checks)
	}
	onlyNoneAndDev(t, run.calls)
}

func TestEnsureVolumeDeviceRestoresDevAfterCancel(t *testing.T) {
	h, run, _ := devHost(t, volLine())
	cs := &ctxScript{script: run}
	h.run = cs
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The caller's context ends right after volmode=none was set, and
	// the first two tries of volmode=dev fail.
	run.answers[setDev] = []any{errors.New("busy"), errors.New("busy"), ""}
	run.onRun = func(call string) {
		if call == setNone {
			cancel()
		}
	}
	_, err := h.EnsureVolumeDevice(ctx, devSpec, stoppedOK)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev, setDev, setDev}) {
		t.Fatalf("sets = %v, want volmode=dev tried until it worked", s)
	}
	for i, e := range cs.ctxErrs[1:] {
		if e != nil {
			t.Errorf("volmode=dev try %d ran with an ended context: %v", i+1, e)
		}
	}
}

func TestEnsureVolumeDeviceRestoresDevWhenNoneFails(t *testing.T) {
	h, run, _ := devHost(t, volLine())
	run.answers[setNone] = &CommandError{Name: "zfs", ExitCode: 1, Stderr: "permission denied", Err: errors.New("exit status 1")}
	if _, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK); err == nil {
		t.Fatal("a failed volmode=none was not reported")
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev}) {
		t.Errorf("sets = %v", s)
	}
}

func TestEnsureVolumeDeviceRestoreFailureNamesTheCommand(t *testing.T) {
	h, run, _ := devHost(t, volLine())
	run.answers[setDev] = errors.New("busy")
	_, err := h.EnsureVolumeDevice(context.Background(), devSpec, stoppedOK)
	me := wantReason(t, err, models.CodeDiskDeviceMissing, "restore_failed")
	if me.Details["command"] != "zfs set volmode=dev "+devDS {
		t.Errorf("details = %v", me.Details)
	}
	if s := sets(run.calls); !reflect.DeepEqual(s, []string{setNone, setDev, setDev, setDev}) {
		t.Errorf("sets = %v, want %d tries of volmode=dev and no second round", s, h.dev.attempts)
	}
}

func TestDisksEnsureDeviceSkipsDirectoryPools(t *testing.T) {
	h, run, _ := devHost(t, volLine())
	h.conf.Directories = []DirPool{{Name: "files", Path: t.TempDir()}}
	d := Disks{conf: h.conf, zfs: h, dirs: &dirs{}}
	res, err := d.EnsureDevice(context.Background(), VolumeSpec{Pool: "files", VMID: vmA, DiskID: diskA1, Slot: 1}, nil)
	if err != nil || res.Rounds != 0 || len(run.calls) != 0 {
		t.Fatalf("= %+v, %v, calls %v", res, err, run.calls)
	}
}
