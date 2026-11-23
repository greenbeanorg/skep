//go:build freebsd

package setup

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// HostProber reads the facts of this FreeBSD host with read-only
// commands: freebsd-version, sysctl, ifconfig (addresses, and each
// bridge's members and flags), zpool list, jls, a
// listing of /dev/vmm, and "service skep_agent|skep_api status".
type HostProber struct{}

// Probe implements Prober.
func (HostProber) Probe(ctx context.Context) (Facts, error) {
	var f Facts
	var err error
	if f.KernelRelease, err = output(ctx, cmdFreeBSDVersion, "-k"); err != nil {
		return f, fmt.Errorf("freebsd-version -k: %w", err)
	}
	if f.UserlandRelease, err = output(ctx, cmdFreeBSDVersion, "-u"); err != nil {
		return f, fmt.Errorf("freebsd-version -u: %w", err)
	}
	if f.Arch, err = output(ctx, cmdSysctl, "-n", "hw.machine_arch"); err != nil {
		return f, fmt.Errorf("sysctl hw.machine_arch: %w", err)
	}
	f.KernelRelease = strings.TrimSpace(f.KernelRelease)
	f.UserlandRelease = strings.TrimSpace(f.UserlandRelease)
	f.Arch = strings.TrimSpace(f.Arch)

	names, err := output(ctx, cmdIfconfig, "-l")
	if err != nil {
		return f, fmt.Errorf("ifconfig -l: %w", err)
	}
	f.Interfaces = map[string][]netip.Prefix{}
	for _, n := range strings.Fields(names) {
		out, err := output(ctx, cmdIfconfig, "-f", "inet:cidr", n, "inet")
		if err != nil {
			// An interface without IPv4 makes ifconfig fail on some
			// releases; it still exists.
			out = ""
		}
		f.Interfaces[n] = parseIfconfigInet(out)
	}
	// Bridges as they are now. "ifconfig -g bridge" fails when there is
	// none on some releases; that is no bridge, not an error. A bridge
	// that cannot be read is an error: setup must not judge it as empty.
	f.Bridges = map[string]Bridge{}
	if out, err := output(ctx, cmdIfconfig, "-g", "bridge"); err == nil {
		for _, n := range strings.Fields(out) {
			bo, err := output(ctx, cmdIfconfig, n)
			if err != nil {
				return f, fmt.Errorf("ifconfig %s: %w", n, err)
			}
			f.Bridges[n] = parseIfconfigBridge(bo)
		}
	}
	// No ZFS (a UFS host without the module) is not an error.
	if out, err := output(ctx, cmdZpool, "list", "-H", "-o", "name,health"); err == nil {
		f.Pools = parseZpoolList(out)
	} else {
		f.Pools = map[string]string{}
	}
	if out, err := output(ctx, cmdJls, "-N", "name"); err == nil {
		f.Jails = lines(out)
	}
	if entries, err := os.ReadDir("/dev/vmm"); err == nil {
		for _, e := range entries {
			f.VMs = append(f.VMs, e.Name())
		}
	}
	f.Running = map[string]bool{}
	for _, svc := range []string{"skep_agent", "skep_api"} {
		if _, err := output(ctx, cmdService, svc, "status"); err == nil {
			f.Running[svc] = true
		}
	}
	return f, nil
}
