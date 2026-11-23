package setup

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Facts is what setup knows about the running host beyond its files. The
// plan is computed from the files under the root and these facts; nothing
// in computing it writes anything.
type Facts struct {
	// KernelRelease and UserlandRelease are freebsd-version -k and -u,
	// for example 15.1-RELEASE-p7.
	KernelRelease   string
	UserlandRelease string
	// Arch is hw.machine_arch, for example amd64.
	Arch string
	// Interfaces are the interfaces that exist now, with their IPv4
	// addresses (prefix form).
	Interfaces map[string][]netip.Prefix
	// Pools maps each imported ZFS pool to its health (ONLINE, ...).
	Pools map[string]string
	// Jails are the names of the running jails; VMs the bhyve VMs in
	// /dev/vmm. Skep's are named skep-<id>.
	Jails []string
	VMs   []string
	// Running holds the Skep services whose "service NAME status"
	// succeeded (skep_agent, skep_api).
	Running map[string]bool
	// Bridges are the bridges that exist now (ifconfig -g bridge), as
	// ifconfig shows them: rc.conf says what a bridge is at the next
	// boot, this what it is now.
	Bridges map[string]Bridge
	// Modules holds, for each module in GuestModules, whether the kernel
	// has it now ("kldstat -q -m NAME"; compiled in counts as loaded).
	// Nil when it could not be read.
	Modules map[string]bool
}

// Bridge is the run-time state of one bridge.
type Bridge struct {
	// Members are the member interfaces ("member:" lines).
	Members []string
	// VLANFilter is whether the bridge flags include VLANFILTER.
	VLANFilter bool
}

// BridgeOf returns the live bridge that has iface as a member, if any.
func (f Facts) BridgeOf(iface string) (string, bool) {
	names := make([]string, 0, len(f.Bridges))
	for n := range f.Bridges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, m := range f.Bridges[n].Members {
			if m == iface {
				return n, true
			}
		}
	}
	return "", false
}

// SkepGuests returns the running Skep jails and VMs.
func (f Facts) SkepGuests() (jails, vms []string) {
	for _, j := range f.Jails {
		if strings.HasPrefix(j, "skep-") {
			jails = append(jails, j)
		}
	}
	for _, v := range f.VMs {
		if strings.HasPrefix(v, "skep-") {
			vms = append(vms, v)
		}
	}
	return jails, vms
}

// OtherGuests returns the running jails and VMs that are not Skep's.
func (f Facts) OtherGuests() (jails, vms []string) {
	for _, j := range f.Jails {
		if !strings.HasPrefix(j, "skep-") {
			jails = append(jails, j)
		}
	}
	for _, v := range f.VMs {
		if !strings.HasPrefix(v, "skep-") {
			vms = append(vms, v)
		}
	}
	return jails, vms
}

// Prober reads the facts of the running host.
type Prober interface {
	Probe(ctx context.Context) (Facts, error)
}

// ErrNotFreeBSD is returned by the prober on any other system.
var ErrNotFreeBSD = errors.New("skep-setup inspects a FreeBSD host; this is not one")

// commandTimeout bounds every read-only command the prober runs.
const commandTimeout = 10 * time.Second

// output runs a fixed command by absolute path (never through a shell),
// with CommandEnv's environment, and returns its standard output.
func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = CommandEnv(false), "/"
	out, err := cmd.Output()
	return string(out), err
}

// parseIfconfigInet reads the "inet A.B.C.D/N" lines of
// "ifconfig -f inet:cidr IF inet".
func parseIfconfigInet(out string) []netip.Prefix {
	var addrs []netip.Prefix
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "inet" {
			if p, err := netip.ParsePrefix(f[1]); err == nil {
				addrs = append(addrs, p)
			}
		}
	}
	return addrs
}

// parseIfconfigBridge reads "ifconfig BRIDGE" on FreeBSD 15.1:
//
//	bridge flags=1<VLANFILTER>
//	member: vtnet1 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
func parseIfconfigBridge(out string) Bridge {
	var b Bridge
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "member:":
			b.Members = append(b.Members, f[1])
		case len(f) >= 2 && f[0] == "bridge" && strings.HasPrefix(f[1], "flags="):
			if i := strings.IndexByte(f[1], '<'); i >= 0 {
				for _, fl := range strings.Split(strings.Trim(f[1][i:], "<>"), ",") {
					if fl == "VLANFILTER" {
						b.VLANFilter = true
					}
				}
			}
		}
	}
	return b
}

// parseZpoolList reads "zpool list -H -o name,health".
func parseZpoolList(out string) map[string]string {
	pools := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			pools[f[0]] = f[1]
		}
	}
	return pools
}

func lines(out string) []string {
	var l []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			l = append(l, s)
		}
	}
	sort.Strings(l)
	return l
}
