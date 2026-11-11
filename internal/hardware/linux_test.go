package hardware

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/pkg/models"
)

// TestLinuxDiscoverDebian13 replays procfs captured from the dev VM.
func TestLinuxDiscoverDebian13(t *testing.T) {
	d := &linuxDiscoverer{
		root: os.DirFS("testdata/linux/debian13"),
		arch: "amd64",
		find: fakeFinder(nil),
		run:  &fakeRunner{},
		now:  fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if hw.Platform != models.PlatformLinuxDev {
		t.Errorf("platform = %q", hw.Platform)
	}
	if hw.Host.Hostname != "merrick" || hw.Host.Architecture != "amd64" {
		t.Errorf("host = %+v", hw.Host)
	}
	os := hw.Host.OS
	if os.Name != "Debian GNU/Linux" || os.Version != "13" ||
		os.Release != "6.12.115+deb13.1-cloud-amd64" || !strings.HasPrefix(os.Kernel, "Linux version 6.12.115") {
		t.Errorf("os = %+v", os)
	}
	wantCPU := models.CPUInfo{Model: "Intel(R) Core(TM) i5-10600K CPU @ 4.10GHz", Cores: 4, Threads: 4}
	if hw.CPU != wantCPU {
		t.Errorf("cpu = %+v", hw.CPU)
	}
	if hw.Memory.TotalBytes != 8138616*1024 || hw.Memory.AvailableBytes != 7217932*1024 {
		t.Errorf("memory = %+v", hw.Memory)
	}
	if b := hw.Hypervisors.Bhyve; b.Status != models.StatusUnavailable || !strings.Contains(b.Reason, "only on FreeBSD") {
		t.Errorf("bhyve must be unavailable on Linux: %+v", b)
	}
	if z := hw.Storage.ZFS; z.Status != models.StatusUnavailable || z.ModuleLoaded {
		t.Errorf("zfs = %+v", z)
	}
	if len(hw.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", hw.Warnings)
	}
}

func TestLinuxDiscoverWithZFSModule(t *testing.T) {
	run := &fakeRunner{outputs: map[string]string{
		"/usr/sbin/zpool list -H -p -o name,size,allocated,free,fragmentation,capacity,health": "tank\t1000\t400\t600\t3\t40\tONLINE\n",
	}}
	d := &linuxDiscoverer{
		root: fstest.MapFS{
			"sys/module/zfs/version": {Data: []byte("2.3.2-1\n")},
		},
		arch: "amd64",
		find: fakeFinder(map[string]string{"zpool": "/usr/sbin/zpool"}),
		run:  run,
		now:  fixedNow,
	}
	hw, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	z := hw.Storage.ZFS
	if z.Status != models.StatusAvailable || z.ModuleVersion != "2.3.2-1" || len(z.Pools) != 1 {
		t.Errorf("zfs = %+v", z)
	}
	// Everything under /proc and /etc is missing in this fixture: discovery
	// must report that as warnings, not as an error.
	if len(hw.Warnings) == 0 {
		t.Error("missing procfs files must produce warnings")
	}
}

func TestParseCPUInfo(t *testing.T) {
	hyperthreaded := `processor	: 0
model name	: Example CPU
physical id	: 0
core id		: 0

processor	: 1
model name	: Example CPU
physical id	: 0
core id		: 0

processor	: 2
model name	: Example CPU
physical id	: 1
core id		: 0

processor	: 3
model name	: Example CPU
physical id	: 1
core id		: 0
`
	got := parseCPUInfo([]byte(hyperthreaded))
	want := models.CPUInfo{Model: "Example CPU", Cores: 2, Threads: 4}
	if got != want {
		t.Errorf("hyperthreaded: got %+v, want %+v", got, want)
	}

	// arm64 kernels report neither a model name nor topology.
	arm := "processor\t: 0\nBogoMIPS\t: 50.00\n\nprocessor\t: 1\nBogoMIPS\t: 50.00\n"
	got = parseCPUInfo([]byte(arm))
	want = models.CPUInfo{Model: "", Cores: 2, Threads: 2}
	if got != want {
		t.Errorf("arm64: got %+v, want %+v", got, want)
	}
}

func TestParseMemInfo(t *testing.T) {
	got, err := parseMemInfo([]byte("MemTotal:       16 kB\nMemFree:  1 kB\nMemAvailable:    8 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalBytes != 16*1024 || got.AvailableBytes != 8*1024 {
		t.Errorf("got %+v", got)
	}
	for _, bad := range []string{
		"",
		"MemTotal: 16 kB\n",
		"MemTotal: lots kB\nMemAvailable: 8 kB\n",
		"MemTotal: 16 MB\nMemAvailable: 8 kB\n",
	} {
		if _, err := parseMemInfo([]byte(bad)); err == nil {
			t.Errorf("parseMemInfo(%q) succeeded, want error", bad)
		}
	}
}

func TestParseOSRelease(t *testing.T) {
	got := parseOSRelease([]byte("# comment\nNAME=\"Debian GNU/Linux\"\nVERSION_ID='13'\nID=debian\nbroken line\n"))
	if got["NAME"] != "Debian GNU/Linux" || got["VERSION_ID"] != "13" || got["ID"] != "debian" || len(got) != 3 {
		t.Errorf("got %v", got)
	}
}
