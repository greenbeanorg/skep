package hardware

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	hearstSysctl = "testdata/freebsd/sysctl-15.1-hearst.txt"
	zpoolListCmd = "/sbin/zpool list -H -p -o name,size,allocated,free,fragmentation,capacity,health"
)

// TestFreeBSDDiscoverHearst replays the state captured from the real FreeBSD
// 15.1 lab VM: vmm not loaded but loadable, zfs loaded with no pools, no QEMU.
func TestFreeBSDDiscoverHearst(t *testing.T) {
	run := &fakeRunner{outputs: map[string]string{zpoolListCmd: ""}}
	d := &freebsdDiscoverer{
		sysctl: loadSysctlFixture(t, hearstSysctl),
		exists: fakeExists("/boot/kernel/vmm.ko", "/boot/kernel/zfs.ko", "/usr/sbin/bhyve"),
		find:   fakeFinder(map[string]string{"zpool": "/sbin/zpool"}),
		run:    run,
		now:    fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if hw.Platform != models.PlatformFreeBSD {
		t.Errorf("platform = %q", hw.Platform)
	}
	wantHost := models.HostInfo{
		Hostname: "hearst.greenbean.org",
		OS: models.OSInfo{
			Name:    "FreeBSD",
			Version: "15.1",
			Release: "15.1-RELEASE-p6",
			Kernel:  "FreeBSD 15.1-RELEASE-p6 releng/15.1-n283643-95719d8a895b GENERIC",
		},
		Architecture: "amd64",
	}
	if hw.Host != wantHost {
		t.Errorf("host = %+v\nwant   %+v", hw.Host, wantHost)
	}
	wantCPU := models.CPUInfo{Model: "Intel(R) Core(TM) i5-10600K CPU @ 4.10GHz", Cores: 2, Threads: 2}
	if hw.CPU != wantCPU {
		t.Errorf("cpu = %+v", hw.CPU)
	}
	wantMem := models.MemoryInfo{TotalBytes: 2107019264, AvailableBytes: (450803 + 1065 + 0) * 4096}
	if hw.Memory != wantMem {
		t.Errorf("memory = %+v, want %+v", hw.Memory, wantMem)
	}

	b := hw.Hypervisors.Bhyve
	if b.Status != models.StatusUnavailable || b.VMMLoaded || !b.VMMLoadable ||
		b.BinaryPath != "/usr/sbin/bhyve" || b.VMMDevice != "" {
		t.Errorf("bhyve = %+v", b)
	}
	if !strings.Contains(b.Reason, "not loaded") {
		t.Errorf("bhyve reason = %q", b.Reason)
	}

	q := hw.Hypervisors.QEMU
	if q.Status != models.StatusUnavailable || len(q.Binaries) != 0 || q.Binaries == nil {
		t.Errorf("qemu = %+v", q)
	}

	z := hw.Storage.ZFS
	if z.Status != models.StatusAvailable || !z.ModuleLoaded ||
		z.ModuleVersion != "2.4.2-1-FreeBSD_g6330a45b0" || z.ZpoolPath != "/sbin/zpool" {
		t.Errorf("zfs = %+v", z)
	}
	if z.Pools == nil || len(z.Pools) != 0 || z.PoolsError != "" {
		t.Errorf("zfs pools = %+v, error %q", z.Pools, z.PoolsError)
	}
	if len(hw.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", hw.Warnings)
	}
	if !hw.CollectedAt.Equal(fixedTime) {
		t.Errorf("collected_at = %v", hw.CollectedAt)
	}
}

// TestFreeBSDDiscoverHearstVMMLoaded replays a second real capture from the
// lab VM, taken with vmm loaded and the file-backed pool skeptest imported.
func TestFreeBSDDiscoverHearstVMMLoaded(t *testing.T) {
	run := &fakeRunner{outputs: map[string]string{
		zpoolListCmd: mustRead(t, "testdata/freebsd/zpool-list-hearst.txt"),
	}}
	d := &freebsdDiscoverer{
		sysctl: loadSysctlFixture(t, "testdata/freebsd/sysctl-15.1-hearst-vmm.txt"),
		// As observed on hearst: /dev/vmmctl exists, /dev/vmm does not
		// (no VM is running).
		exists: fakeExists("/boot/kernel/vmm.ko", "/boot/kernel/zfs.ko", "/usr/sbin/bhyve", "/dev/vmmctl"),
		find:   fakeFinder(map[string]string{"zpool": "/sbin/zpool"}),
		run:    run,
		now:    fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	b := hw.Hypervisors.Bhyve
	if b.Status != models.StatusAvailable || !b.VMMLoaded || !b.VMMLoadable ||
		b.VMMDevice != "/dev/vmmctl" || b.BinaryPath != "/usr/sbin/bhyve" {
		t.Errorf("bhyve = %+v", b)
	}
	z := hw.Storage.ZFS
	if z.Status != models.StatusAvailable || len(z.Pools) != 1 || z.PoolsError != "" {
		t.Fatalf("zfs = %+v", z)
	}
	p := z.Pools[0]
	if p.Name != "skeptest" || p.SizeBytes != 2013265920 || p.AllocatedBytes != 122880 ||
		p.FreeBytes != 2013143040 || p.Health != "ONLINE" {
		t.Errorf("pool = %+v", p)
	}
	if p.FragmentationPercent == nil || *p.FragmentationPercent != 1 ||
		p.CapacityPercent == nil || *p.CapacityPercent != 0 {
		t.Errorf("pool percentages = %v %v", p.FragmentationPercent, p.CapacityPercent)
	}
	if len(hw.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", hw.Warnings)
	}
}

// TestFreeBSDDiscoverFullHost models a host with vmm loaded, pools imported,
// and QEMU installed. The vmm sysctl, zpool, and QEMU values are synthetic.
func TestFreeBSDDiscoverFullHost(t *testing.T) {
	sysctl := loadSysctlFixture(t, hearstSysctl)
	sysctl.values["hw.vmm.maxcpu"] = "256"
	sysctl.values["kern.smp.cores"] = "6"
	sysctl.values["hw.ncpu"] = "12"

	run := &fakeRunner{outputs: map[string]string{
		zpoolListCmd: mustRead(t, "testdata/freebsd/zpool-list-synthetic.txt"),
		"/usr/local/bin/qemu-system-x86_64 --version": mustRead(t, "testdata/qemu/system-version.txt"),
		"/usr/local/bin/qemu-img --version":           mustRead(t, "testdata/qemu/img-version.txt"),
	}}
	d := &freebsdDiscoverer{
		sysctl: sysctl,
		exists: fakeExists("/boot/kernel/vmm.ko", "/usr/sbin/bhyve", "/dev/vmmctl", "/dev/vmm"),
		find: fakeFinder(map[string]string{
			"zpool":              "/sbin/zpool",
			"qemu-system-x86_64": "/usr/local/bin/qemu-system-x86_64",
			"qemu-img":           "/usr/local/bin/qemu-img",
		}),
		run: run,
		now: fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if hw.CPU.Cores != 6 || hw.CPU.Threads != 12 {
		t.Errorf("cpu = %+v", hw.CPU)
	}
	b := hw.Hypervisors.Bhyve
	if b.Status != models.StatusAvailable || !b.VMMLoaded || b.VMMDevice != "/dev/vmmctl" {
		t.Errorf("bhyve = %+v", b)
	}
	q := hw.Hypervisors.QEMU
	if q.Status != models.StatusAvailable || q.Acceleration != "tcg" || len(q.Binaries) != 2 {
		t.Fatalf("qemu = %+v", q)
	}
	for _, bin := range q.Binaries {
		if bin.Version != "9.2.0" || bin.Error != "" {
			t.Errorf("qemu binary = %+v", bin)
		}
	}
	z := hw.Storage.ZFS
	if z.Status != models.StatusAvailable || len(z.Pools) != 2 {
		t.Fatalf("zfs = %+v", z)
	}
	if z.Pools[0].Name != "zroot" || z.Pools[1].Health != "DEGRADED" {
		t.Errorf("pools = %+v", z.Pools)
	}
}

func TestFreeBSDBhyveStates(t *testing.T) {
	tests := []struct {
		name       string
		vmmLoaded  bool
		sysctlErr  error
		exists     []string
		wantStatus models.CapabilityStatus
		wantReason string
	}{
		{"loaded with binary", true, nil, []string{"/usr/sbin/bhyve"}, models.StatusAvailable, "is loaded"},
		{"loaded without binary", true, nil, nil, models.StatusUnavailable, "binary not found"},
		{"loadable", false, nil, []string{"/usr/sbin/bhyve", "/boot/modules/vmm.ko"}, models.StatusUnavailable, "can be loaded"},
		{"module missing", false, nil, []string{"/usr/sbin/bhyve"}, models.StatusUnavailable, "was not found"},
		{"sysctl failure", false, errors.New("permission denied"), []string{"/usr/sbin/bhyve"}, models.StatusUnknown, "permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysctl := &fakeSysctl{values: map[string]string{}, errs: map[string]error{}}
			if tt.vmmLoaded {
				sysctl.values["hw.vmm.maxcpu"] = "256"
			}
			if tt.sysctlErr != nil {
				sysctl.errs["hw.vmm.maxcpu"] = tt.sysctlErr
			}
			d := &freebsdDiscoverer{sysctl: sysctl, exists: fakeExists(tt.exists...)}
			got := d.bhyve()
			if got.Status != tt.wantStatus || !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("bhyve = %+v, want status %q and reason containing %q", got, tt.wantStatus, tt.wantReason)
			}
		})
	}
}

// TestFreeBSDNeverRunsZpoolWhenZFSNotLoaded guards the read-only rule:
// zpool(8) can load zfs.ko as a side effect, so it must not be run.
func TestFreeBSDNeverRunsZpoolWhenZFSNotLoaded(t *testing.T) {
	sysctl := loadSysctlFixture(t, hearstSysctl)
	delete(sysctl.values, "vfs.zfs.version.module")
	run := &fakeRunner{}
	d := &freebsdDiscoverer{
		sysctl: sysctl,
		exists: fakeExists("/boot/kernel/zfs.ko"),
		find:   fakeFinder(map[string]string{"zpool": "/sbin/zpool"}),
		run:    run,
		now:    fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	z := hw.Storage.ZFS
	if z.Status != models.StatusUnavailable || z.ModuleLoaded || !strings.Contains(z.Reason, "can be loaded") {
		t.Errorf("zfs = %+v", z)
	}
	if run.ran("/sbin/zpool") {
		t.Errorf("zpool was executed although zfs is not loaded: %v", run.calls)
	}
}

// TestFreeBSDDiscoverSurvivesMissingSysctls checks that discovery degrades
// to warnings and unknown values instead of failing.
func TestFreeBSDDiscoverSurvivesMissingSysctls(t *testing.T) {
	d := &freebsdDiscoverer{
		sysctl: &fakeSysctl{values: map[string]string{}, errs: map[string]error{
			"vfs.zfs.version.module": errors.New("boom"),
		}},
		exists: fakeExists(),
		find:   fakeFinder(nil),
		run:    &fakeRunner{},
		now:    fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("discovery must not fail outright: %v", err)
	}
	if len(hw.Warnings) == 0 {
		t.Error("missing sysctls must be reported as warnings")
	}
	if hw.Storage.ZFS.Status != models.StatusUnknown {
		t.Errorf("zfs = %+v", hw.Storage.ZFS)
	}
	if hw.Hypervisors.Bhyve.Status != models.StatusUnavailable {
		t.Errorf("bhyve = %+v", hw.Hypervisors.Bhyve)
	}
}

func TestDiscoverHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &freebsdDiscoverer{sysctl: &fakeSysctl{}, exists: fakeExists(), find: fakeFinder(nil), run: &fakeRunner{}, now: fixedNow}
	if _, err := d.Discover(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestFreeBSDVersion(t *testing.T) {
	for in, want := range map[string]string{
		"15.1-RELEASE-p6": "15.1",
		"15.1-RELEASE":    "15.1",
		"16.0-CURRENT":    "16.0",
		"":                "",
	} {
		if got := freebsdVersion(in); got != want {
			t.Errorf("freebsdVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
