package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

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
func startedJail(t *testing.T) (*host, *script, string) {
	t.Helper()
	h, run, _ := jailHost(t)
	root := "/skeptest/skep/jail/" + ctrA
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
	if run.outputs[len(run.outputs)-1] != logPath {
		t.Fatalf("rc output went to %q, want %q", run.outputs[len(run.outputs)-1], logPath)
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
