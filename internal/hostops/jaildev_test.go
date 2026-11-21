package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// devStep7 is the /dev of a ruleset 5 jail on hearst (step 7).
var devStep7 = strings.Fields("fd null pts random stderr stdin stdout urandom zero zfs")

// devIncludeFive is the /dev of the DHCP jail n8dhcp under amendment h's
// ruleset 6001 (include 5), as recorded on hearst
// (tests/integration/jails/results/step8/devfs-include.txt).
var devIncludeFive = strings.Fields(`acpi apm apmctl atkbd0 audit auditpipe bpf bpf0 bpsm0 cd0 console consolectl ctty
da0 da0s1 da0s1a da0s1b devctl devctl2 devstat fd fido full geom.ctl hpet0 input io iso9660 kbd0 kbd1 kbdmux0 klog
kmem mdctl mem mlx5ctl netdump netmap null pass0 pass1 pci pfil psm0 pts pvclock random reroot sndstat stderr stdin
stdout sysmouse tcp_log ttyV0.1 ttyv0 ttyv1 ttyv2 ttyv3 ttyv4 ttyv5 ttyv6 ttyv7 ttyv8 ttyv9 ttyva ttyvb ufssuspend
ugen0.1 ugen1.1 ugen2.1 ugen3.1 ugen4.1 ugen5.1 ugen6.1 ugen7.1 ugen7.2 uinput urandom usb usbctl vmmctl vtcon xpt0
zero zfs zvol`)

func TestCheckJailDev(t *testing.T) {
	ok := map[string]struct {
		entries []string
		dhcp    bool
	}{
		"step 7 static jail":      {devStep7, false},
		"step 7 jail with DHCP":   {devStep7, true},
		"DHCP jail with bpf":      {append(slices.Clone(devStep7), "bpf", "bpf0", "bpf12"), true},
		"the whole allow-list":    {strings.Fields("fd null zero crypto random urandom pts ptmx stdin stdout stderr zfs fuse pf"), false},
		"DHCP, the allow-list":    {strings.Fields("fd null zero crypto random urandom pts ptmx stdin stdout stderr zfs fuse pf bpf bpf0"), true},
		"order does not matter":   {[]string{"zero", "null"}, false},
		"bpf alone with DHCP too": {[]string{"null", "bpf"}, true},
	}
	for name, c := range ok {
		if err := checkJailDev(ctrA, c.entries, c.dhcp); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	bad := map[string]struct {
		entries []string
		dhcp    bool
		extra   []string
	}{
		"amendment h's DHCP jail": {devIncludeFive, true, nil},
		"bpf in a static jail":    {append(slices.Clone(devStep7), "bpf0"), false, []string{"bpf0"}},
		"one extra device":        {append(slices.Clone(devStep7), "mem"), false, []string{"mem"}},
		"a disk in a DHCP jail":   {append(slices.Clone(devStep7), "bpf", "da0"), true, []string{"da0"}},
		"a bpf look-alike":        {append(slices.Clone(devStep7), "bpfx"), true, []string{"bpfx"}},
		"a directory of devices":  {append(slices.Clone(devStep7), "usb"), false, []string{"usb"}},
		"no devfs":                {nil, false, nil},
		"an empty directory":      {[]string{}, true, nil},
	}
	for name, c := range bad {
		err := checkJailDev(ctrA, c.entries, c.dhcp)
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeContainerStartFailed {
			t.Errorf("%s: error = %v, want container_start_failed", name, err)
			continue
		}
		if c.extra != nil && !slices.Equal(me.Details["extra_devices"].([]string), c.extra) {
			t.Errorf("%s: extra = %v, want %v", name, me.Details["extra_devices"], c.extra)
		}
	}
	// The recorded leak names what it found, sorted.
	err := checkJailDev(ctrA, devIncludeFive, true)
	for _, want := range []string{"da0", "kmem", "mem", "io", "pass0", "netmap"} {
		if !strings.Contains(err.Error(), " "+want+" ") && !strings.HasSuffix(err.Error(), " "+want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

// startedJail scripts what a jail just created by jail -c looks like: jls
// reports good parameters, and nothing is moved in (no NICs).
// testJailRoot is the root of the started jail of the tests.
const testJailRoot = "/skeptest/skep/jail/" + ctrA

func startedJail(t *testing.T) (*host, *script, string) {
	t.Helper()
	h, run, _ := jailHost(t)
	root := testJailRoot
	raw, err := json.Marshal(map[string]any{"jail-information": map[string]any{"jail": []any{goodParams(root)}}})
	if err != nil {
		t.Fatal(err)
	}
	name := "skep-" + ctrA
	run.answers["jls --libxo=json -n -j "+name] = string(raw)
	// Present until jail -r runs.
	run.answers["jls -j "+name+" jid"] = "12\n"
	run.onRun = func(call string) {
		if strings.HasPrefix(call, "jail ") && strings.HasSuffix(call, " -r "+name) {
			run.answers["jls -j "+name+" jid"] = &CommandError{Name: "jls", ExitCode: 1, Stderr: "jls: jail \"" + name + "\" not found", Err: errors.New("exit status 1")}
		}
	}
	return h, run, root
}

func callsWith(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestFinishStartRunsRCAfterTheGuard(t *testing.T) {
	h, run, root := startedJail(t)
	listed := false
	h.listDev = func(r string) ([]string, error) {
		if r != root {
			t.Errorf("listed %s, want %s", r, root)
		}
		if slices.ContainsFunc(run.calls, func(c string) bool { return strings.HasPrefix(c, "jexec") }) {
			t.Error("/etc/rc ran before the /dev guard")
		}
		listed = true
		return devStep7, nil
	}
	name := "skep-" + ctrA
	conf, err := h.writeJailConfAt(jailSpec(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	jid, err := h.finishStart(context.Background(), jailSpec(), name, conf, root)
	if err != nil || jid != 12 {
		t.Fatalf("finishStart = %d, %v", jid, err)
	}
	if !listed {
		t.Fatal("the /dev guard did not run")
	}
	rc := callsWith(run.calls, "jexec")
	if !slices.Equal(rc, []string{"jexec -l -U root " + name + " /bin/sh /etc/rc"}) {
		t.Fatalf("rc calls: %q", rc)
	}
	logPath, _ := h.conf.JailRunPath(ctrA, JailConsoleLog)
	if got := run.envCmds[len(run.envCmds)-1].ConsoleLog; got != logPath {
		t.Fatalf("rc output went to %q, want %q", got, logPath)
	}
	if r := callsWith(run.calls, "jail "); len(r) != 0 {
		t.Fatalf("a good start removed the jail: %q", r)
	}
}

func TestFinishStartGuardRefusesWithoutRunningJailCode(t *testing.T) {
	for name, list := range map[string]func(string) ([]string, error){
		"extra device": func(string) ([]string, error) { return append(slices.Clone(devStep7), "mem"), nil },
		"no devfs":     func(string) ([]string, error) { return nil, errors.New("the jail's /dev is ufs") },
	} {
		h, run, root := startedJail(t)
		h.listDev = list
		jname := "skep-" + ctrA
		conf, err := h.writeJailConfAt(jailSpec(), root, true)
		if err != nil {
			t.Fatal(err)
		}
		_, serr := h.finishStart(context.Background(), jailSpec(), jname, conf, root)
		wantCode(t, serr, models.CodeContainerStartFailed)
		if rc := callsWith(run.calls, "jexec"); len(rc) != 0 {
			t.Fatalf("%s: /etc/rc ran in a refused jail: %q", name, rc)
		}
		if r := callsWith(run.calls, "jail "); !slices.Equal(r, []string{"jail -f " + conf + " -r " + jname}) {
			t.Fatalf("%s: removal: %q", name, r)
		}
		// The jail.conf jail -r read has no exec.stop: none of the
		// jail's code runs, /etc/rc.shutdown included.
		data, err := os.ReadFile(conf)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "exec.stop") {
			t.Fatalf("%s: the refused jail was removed with exec.stop:\n%s", name, data)
		}
		if name == "extra device" && !strings.Contains(serr.Error(), "mem") {
			t.Fatalf("the refusal does not name the device: %v", serr)
		}
	}
}

func TestFinishStartRCFailureStopsWithShutdown(t *testing.T) {
	h, run, root := startedJail(t)
	h.listDev = func(string) ([]string, error) { return devStep7, nil }
	name := "skep-" + ctrA
	run.answers["jexec -l -U root "+name+" /bin/sh /etc/rc"] = errors.New("hostops: jexec timed out after 2m0s")
	conf, err := h.writeJailConfAt(jailSpec(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.finishStart(context.Background(), jailSpec(), name, conf, root)
	wantCode(t, err, models.CodeContainerStartFailed)
	if !strings.Contains(err.Error(), "console log") {
		t.Fatalf("the failure does not point at the console log: %v", err)
	}
	if r := callsWith(run.calls, "jail "); !slices.Equal(r, []string{"jail -f " + conf + " -r " + name}) {
		t.Fatalf("removal: %q", r)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\texec.stop = \"/bin/sh /etc/rc.shutdown jail\";\n") {
		t.Fatalf("a jail whose rc failed was removed without exec.stop:\n%s", data)
	}
}

func TestFinishStartBadParamsSkipGuardAndRC(t *testing.T) {
	h, run, root := startedJail(t)
	h.listDev = func(string) ([]string, error) {
		t.Error("the guard ran for a jail with the wrong parameters")
		return devStep7, nil
	}
	spec := jailSpec()
	spec.SysVIPC = true // jls reports disable
	conf, err := h.writeJailConfAt(spec, root, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.finishStart(context.Background(), spec, "skep-"+ctrA, conf, root)
	wantCode(t, err, models.CodeContainerStartFailed)
	if rc := callsWith(run.calls, "jexec"); len(rc) != 0 {
		t.Fatalf("rc ran: %q", rc)
	}
}

// nicJail is startedJail with one static NIC whose jail end jail -c moved
// in, so it is gone from the host.
func nicJail(t *testing.T) (*host, *script, string, JailSpec) {
	t.Helper()
	h, run, root := startedJail(t)
	spec := jailSpec()
	spec.NICs = []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:5e:00:00:01"}, {Slot: 1, Bridge: "bridge0", MAC: "02:00:5e:00:00:02"}}
	for slot := range 2 {
		jif, _ := JailInterfaceName(ctrA, slot)
		run.answers["ifconfig "+jif] = noSuchInterface(jif)
		guest, _ := GuestInterfaceName(slot)
		run.answers["ifconfig -j skep-"+ctrA+" "+jif+" name "+guest] = guest + "\n"
	}
	return h, run, root, spec
}

func TestFinishStartRenamesInJailBeforeTheGuard(t *testing.T) {
	h, run, root, spec := nicJail(t)
	name := "skep-" + ctrA
	h.listDev = func(string) ([]string, error) {
		if n := len(callsWith(run.calls, "ifconfig -j ")); n != 2 {
			t.Errorf("the /dev guard ran after %d renames, want 2", n)
		}
		return devStep7, nil
	}
	conf, err := h.writeJailConfAt(spec, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.finishStart(context.Background(), spec, name, conf, root); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ifconfig -j " + name + " sjc0c0c0c011n0 name skep0",
		"ifconfig -j " + name + " sjc0c0c0c011n1 name skep1",
	}
	if got := callsWith(run.calls, "ifconfig -j "); !slices.Equal(got, want) {
		t.Fatalf("renames: %q", got)
	}
	// Moved in first, then renamed, then /etc/rc.
	moved := slices.Index(run.calls, "ifconfig sjc0c0c0c011n1")
	renamed := slices.Index(run.calls, want[0])
	rc := slices.Index(run.calls, "jexec -l -U root "+name+" /bin/sh /etc/rc")
	if moved < 0 || renamed < moved || rc < renamed {
		t.Fatalf("order: moved %d, renamed %d, rc %d: %q", moved, renamed, rc, run.calls)
	}
}

func TestFinishStartRenameFailureRemovesWithoutRC(t *testing.T) {
	for what, answer := range map[string]any{
		"error":      &CommandError{Name: "ifconfig", ExitCode: 1, Stderr: "ifconfig: ioctl SIOCSIFNAME (set name): File exists", Err: errors.New("exit status 1")},
		"wrong name": "skep9\n",
	} {
		h, run, root, spec := nicJail(t)
		name := "skep-" + ctrA
		run.answers["ifconfig -j "+name+" sjc0c0c0c011n1 name skep1"] = answer
		h.listDev = func(string) ([]string, error) {
			t.Errorf("%s: the guard ran after a failed rename", what)
			return devStep7, nil
		}
		conf, err := h.writeJailConfAt(spec, root, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.finishStart(context.Background(), spec, name, conf, root)
		wantCode(t, err, models.CodeContainerStartFailed)
		if !strings.Contains(err.Error(), "skep1") {
			t.Fatalf("%s: the refusal does not name the interface: %v", what, err)
		}
		if rc := callsWith(run.calls, "jexec"); len(rc) != 0 {
			t.Fatalf("%s: rc ran: %q", what, rc)
		}
		if r := callsWith(run.calls, "jail "); !slices.Equal(r, []string{"jail -f " + conf + " -r " + name}) {
			t.Fatalf("%s: removal: %q", what, r)
		}
		data, _ := os.ReadFile(conf)
		if strings.Contains(string(data), "exec.stop") {
			t.Fatalf("%s: removed with exec.stop", what)
		}
	}
}

// Every removal Skep makes (stop, discard, failed /etc/rc) destroys the
// epairs before jail -r, so no jail end returns to the host named
// skep<slot> (amendment k).
func TestRemoveJailDestroysEpairsFirst(t *testing.T) {
	h, run, root := startedJail(t)
	run.answers["ifconfig -g epair"] = "epair0a\nepair0b\n"
	run.answers["ifconfig epair0a"] = ifconfigShow("epair0a", "skep:"+ctrA+":0")
	name := "skep-" + ctrA
	conf, err := h.writeJailConfAt(jailSpec(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.removeJail(context.Background(), name, conf, testJailRoot, jailSpec(), true); err != nil {
		t.Fatal(err)
	}
	destroy := slices.Index(run.calls, "ifconfig epair0a destroy")
	remove := slices.Index(run.calls, "jail -f "+conf+" -r "+name)
	if destroy < 0 || remove < 0 || destroy > remove {
		t.Fatalf("destroy at %d, jail -r at %d: %q", destroy, remove, run.calls)
	}
}

// TestFinishStartRefusalWithFailedRemovalIsQuarantined: when the guard
// refuses the jail and its removal then fails, the start reports
// container_quarantined, /etc/rc never ran, and the only removal tried
// was one without exec.stop (amendment n, SR-06).
func TestFinishStartRefusalWithFailedRemovalIsQuarantined(t *testing.T) {
	h, run, root := startedJail(t)
	h.listDev = func(string) ([]string, error) { return append(slices.Clone(devStep7), "mem"), nil }
	name := "skep-" + ctrA
	conf, err := h.writeJailConfAt(jailSpec(), root, true)
	if err != nil {
		t.Fatal(err)
	}
	// jail -r times out and the forced removal, jail -R, fails too
	// (amendment u): only then is the jail quarantined.
	run.answers["jail -f "+conf+" -r "+name] = &TimeoutError{Name: "jail", After: 160 * time.Second}
	run.answers["jail -R "+name] = &CommandError{Name: "jail", ExitCode: 1, Stderr: "jail: busy", Err: errors.New("exit status 1")}
	_, err = h.finishStart(context.Background(), jailSpec(), name, conf, root)
	wantCode(t, err, models.CodeContainerQuarantined)
	if !slices.Contains(run.calls, "jail -R "+name) {
		t.Fatalf("no forced removal: %q", run.calls)
	}
	if !strings.Contains(err.Error(), "mem") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the refusal does not give both causes: %v", err)
	}
	if rc := callsWith(run.calls, "jexec"); len(rc) != 0 {
		t.Fatalf("/etc/rc ran in a quarantined jail: %q", rc)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "exec.stop") {
		t.Fatalf("the quarantined jail's removal had exec.stop:\n%s", data)
	}
}
