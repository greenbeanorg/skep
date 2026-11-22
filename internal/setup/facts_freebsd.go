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
// commands: freebsd-version, sysctl, ifconfig, zpool list, jls, and a
// listing of /dev/vmm.
type HostProber struct{}

// Probe implements Prober.
func (HostProber) Probe(ctx context.Context) (Facts, error) {
	var f Facts
	var err error
	if f.KernelRelease, err = output(ctx, "freebsd-version", "-k"); err != nil {
		return f, fmt.Errorf("freebsd-version -k: %w", err)
	}
	if f.UserlandRelease, err = output(ctx, "freebsd-version", "-u"); err != nil {
		return f, fmt.Errorf("freebsd-version -u: %w", err)
	}
	if f.Arch, err = output(ctx, "sysctl", "-n", "hw.machine_arch"); err != nil {
		return f, fmt.Errorf("sysctl hw.machine_arch: %w", err)
	}
	f.KernelRelease = strings.TrimSpace(f.KernelRelease)
	f.UserlandRelease = strings.TrimSpace(f.UserlandRelease)
	f.Arch = strings.TrimSpace(f.Arch)

	names, err := output(ctx, "ifconfig", "-l")
	if err != nil {
		return f, fmt.Errorf("ifconfig -l: %w", err)
	}
	f.Interfaces = map[string][]netip.Prefix{}
	for _, n := range strings.Fields(names) {
		out, err := output(ctx, "ifconfig", "-f", "inet:cidr", n, "inet")
		if err != nil {
			// An interface without IPv4 makes ifconfig fail on some
			// releases; it still exists.
			out = ""
		}
		f.Interfaces[n] = parseIfconfigInet(out)
	}
	// No ZFS (a UFS host without the module) is not an error.
	if out, err := output(ctx, "zpool", "list", "-H", "-o", "name,health"); err == nil {
		f.Pools = parseZpoolList(out)
	} else {
		f.Pools = map[string]string{}
	}
	if out, err := output(ctx, "jls", "-N", "name"); err == nil {
		f.Jails = lines(out)
	}
	if entries, err := os.ReadDir("/dev/vmm"); err == nil {
		for _, e := range entries {
			f.VMs = append(f.VMs, e.Name())
		}
	}
	return f, nil
}
