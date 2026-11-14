package hostops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// What Capabilities reads. Reading them changes nothing.
const (
	// sysctlVMMMaxCPU exists only while the vmm module is loaded.
	sysctlVMMMaxCPU = "hw.vmm.maxcpu"
	sysctlNCPU      = "hw.ncpu"
	sysctlPhysMem   = "hw.physmem"
	// vmmDeviceDir holds one node per vmm instance.
	vmmDeviceDir = "/dev/vmm"
)

// Capabilities implements VMM.
func (h *host) Capabilities(ctx context.Context) VMMInfo {
	info := VMMInfo{
		BhyvePresent: h.sys.Exists(bhyveBinary),
		FirmwareCode: h.sys.Exists(filepath.Join(h.conf.FirmwareDir, FirmwareCodeFile)),
		FirmwareVars: h.sys.Exists(filepath.Join(h.conf.FirmwareDir, FirmwareVarsFile)),
	}
	if n, err := h.sys.SysctlUint64(sysctlVMMMaxCPU); err == nil {
		info.VMMLoaded, info.MaxCPU = true, int(n)
	} else if !errors.Is(err, errNoSuchOID) {
		h.log.Warn("hostops: cannot read "+sysctlVMMMaxCPU, "error", err)
	}
	if n, err := h.sys.SysctlUint64(sysctlNCPU); err == nil {
		info.HostCPUs = int(n)
	}
	if n, err := h.sys.SysctlUint64(sysctlPhysMem); err == nil {
		info.HostMemoryBytes = int64(n)
	}
	// kldstat -q -m exits 0 when the module is loaded or compiled into
	// the kernel. It loads nothing.
	if _, err := h.run.Run(ctx, kldstatBinary, "-q", "-m", "nmdm"); err == nil {
		info.NmdmAvailable = true
	}
	return info
}

// InstanceExists implements VMM.
func (h *host) InstanceExists(vmID string) bool {
	name, err := Instance(vmID)
	if err != nil {
		return false
	}
	return h.sys.Exists(filepath.Join(vmmDeviceDir, name))
}

// Bridge implements Net.
func (h *host) Bridge(ctx context.Context, name string) (Bridge, error) {
	if err := h.conf.CheckBridge(name); err != nil {
		return Bridge{}, err
	}
	out, err := h.run.Run(ctx, ifconfigBinary, name)
	if err != nil {
		if stderrContains(err, "does not exist") {
			return Bridge{Name: name}, nil
		}
		return Bridge{}, err
	}
	return parseBridge(name, out), nil
}

// parseBridge reads the flags and the members from ifconfig(8) output.
func parseBridge(name string, out []byte) Bridge {
	b := Bridge{Name: name, Exists: true, Members: []string{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for first := true; sc.Scan(); first = false {
		line := sc.Text()
		if first {
			// bridge0: flags=1008843<UP,BROADCAST,...> metric 0 mtu 1500
			if _, rest, ok := strings.Cut(line, "<"); ok {
				flags, _, _ := strings.Cut(rest, ">")
				for _, f := range strings.Split(flags, ",") {
					if f == "UP" {
						b.Up = true
					}
				}
			}
			continue
		}
		// 	member: vtnet1 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "member: "); ok {
			member, _, _ := strings.Cut(rest, " ")
			if member != "" {
				b.Members = append(b.Members, member)
			}
		}
	}
	return b
}
