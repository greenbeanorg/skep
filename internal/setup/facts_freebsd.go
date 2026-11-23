//go:build freebsd

package setup

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// HostProber reads the facts of this FreeBSD host with read-only
// commands: freebsd-version, sysctl, ifconfig (addresses, and each
// bridge's members and flags), zpool list, jls, a
// listing of /dev/vmm, "service skep_agent|skep_api status", and
// "kldstat -q -m" for each guest module. Facts.Kenv runs "/bin/kenv -q
// NAME" when a caller asks for a loader tunable.
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
	if f.Bridges, err = readBridges(ctx, output); err != nil {
		return f, err
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
	f.Modules = probeModules(ctx)
	f.Kenv = func(name string) (string, bool) { return readKenv(ctx, name) }
	f.Running = map[string]bool{}
	for _, svc := range []string{"skep_agent", "skep_api"} {
		if _, err := output(ctx, cmdService, svc, "status"); err == nil {
			f.Running[svc] = true
		}
	}
	return f, nil
}

// readKenv reads one kernel environment variable with /bin/kenv -q, read
// only. The name must be a plain tunable name; anything else, an absent
// variable, or a failure is ok == false.
func readKenv(ctx context.Context, name string) (string, bool) {
	if !tunableNameRE.MatchString(name) {
		return "", false
	}
	out, err := output(ctx, cmdKenv, "-q", name)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(out, "\n"), true
}

// probeModules asks the kernel about each guest module: kldstat -q -m
// exits 0 when the module is present (loaded or compiled in) and 1 when it
// is not. Any other outcome makes the whole answer unknown (nil): setup
// never reports a module as missing on a guess.
func probeModules(ctx context.Context) map[string]bool {
	m := map[string]bool{}
	for _, name := range GuestModules {
		_, err := output(ctx, cmdKldstat, "-q", "-m", name)
		var ee *exec.ExitError
		switch {
		case err == nil:
			m[name] = true
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			m[name] = false
		default:
			return nil
		}
	}
	return m
}
