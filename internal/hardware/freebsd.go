package hardware

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// This file holds the FreeBSD discovery logic. It has no build tag: it only
// talks to the system through SysctlReader, existsFunc, pathFinder, and
// CommandRunner, so it is unit tested on Linux with fixtures captured from a
// real FreeBSD host. The real sysctl binding is in sysctl_freebsd.go.

// ErrSysctlNotFound is returned by a SysctlReader when the OID does not
// exist, which on FreeBSD usually means a kernel module is not loaded.
var ErrSysctlNotFound = errors.New("sysctl oid not found")

// SysctlReader reads FreeBSD sysctl values by name.
type SysctlReader interface {
	// String returns a string-typed sysctl.
	String(name string) (string, error)
	// Uint64 returns an integer-typed sysctl of any width.
	Uint64(name string) (uint64, error)
}

// Paths checked by FreeBSD discovery. Checking them is read-only.
const (
	bhyveBinary = "/usr/sbin/bhyve"
	// vmmCtlDevice exists while the vmm module is loaded (FreeBSD 15+).
	vmmCtlDevice = "/dev/vmmctl"
	// vmmDeviceDir holds one node per running VM and may be absent even
	// when vmm is loaded.
	vmmDeviceDir = "/dev/vmm"
)

var (
	vmmModulePaths = []string{"/boot/kernel/vmm.ko", "/boot/modules/vmm.ko"}
	zfsModulePaths = []string{"/boot/kernel/zfs.ko", "/boot/modules/zfs.ko"}
)

// Sysctl OIDs that exist only while the corresponding module is loaded (or
// compiled into the kernel).
const (
	sysctlVMMLoaded  = "hw.vmm.maxcpu"
	sysctlZFSVersion = "vfs.zfs.version.module"
)

type freebsdDiscoverer struct {
	sysctl SysctlReader
	exists existsFunc
	find   pathFinder
	run    CommandRunner
	now    func() time.Time
}

func (d *freebsdDiscoverer) Discover(ctx context.Context) (models.Hardware, error) {
	if err := ctx.Err(); err != nil {
		return models.Hardware{}, err
	}
	var w warnings

	release := d.str("kern.osrelease", &w)
	threads := int(d.num("hw.ncpu", &w))
	cores := threads
	if n, err := d.sysctl.Uint64("kern.smp.cores"); err == nil && n > 0 {
		cores = int(n)
	} else {
		w.add("kern.smp.cores unavailable; reporting logical CPUs as cores")
	}

	hw := models.Hardware{
		Platform: models.PlatformFreeBSD,
		Host: models.HostInfo{
			Hostname: d.str("kern.hostname", &w),
			OS: models.OSInfo{
				Name:    d.str("kern.ostype", &w),
				Version: freebsdVersion(release),
				Release: release,
				Kernel:  d.str("kern.version", &w),
			},
			Architecture: d.str("hw.machine_arch", &w),
		},
		CPU: models.CPUInfo{
			Model:   d.str("hw.model", &w),
			Cores:   cores,
			Threads: threads,
		},
		Memory: d.memory(&w),
		Hypervisors: models.Hypervisors{
			Bhyve: d.bhyve(),
			QEMU:  discoverQEMU(ctx, d.find, d.run),
		},
		Storage: models.StorageInfo{
			ZFS: discoverZFS(ctx, d.zfsKernel(), d.find, d.run),
		},
		CollectedAt: d.now().UTC(),
	}
	hw.Warnings = w.list()

	if err := ctx.Err(); err != nil {
		return models.Hardware{}, err
	}
	return hw, nil
}

func (d *freebsdDiscoverer) str(name string, w *warnings) string {
	v, err := d.sysctl.String(name)
	if err != nil {
		w.add(fmt.Sprintf("sysctl %s: %v", name, err))
		return ""
	}
	return strings.TrimSpace(v)
}

func (d *freebsdDiscoverer) num(name string, w *warnings) uint64 {
	v, err := d.sysctl.Uint64(name)
	if err != nil {
		w.add(fmt.Sprintf("sysctl %s: %v", name, err))
		return 0
	}
	return v
}

// memory reports physical memory and an estimate of available memory: the
// free, inactive, and laundry page queues, which the kernel can reclaim
// without swapping out active pages.
func (d *freebsdDiscoverer) memory(w *warnings) models.MemoryInfo {
	pageSize := d.num("hw.pagesize", w)
	pages := d.num("vm.stats.vm.v_free_count", w) +
		d.num("vm.stats.vm.v_inactive_count", w) +
		d.num("vm.stats.vm.v_laundry_count", w)
	return models.MemoryInfo{
		TotalBytes:     d.num("hw.physmem", w),
		AvailableBytes: pages * pageSize,
	}
}

// oidPresent reports whether a sysctl OID exists.
func (d *freebsdDiscoverer) oidPresent(name string) (bool, error) {
	_, err := d.sysctl.Uint64(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrSysctlNotFound):
		return false, nil
	default:
		return false, err
	}
}

func (d *freebsdDiscoverer) anyExists(paths []string) bool {
	for _, p := range paths {
		if d.exists(p) {
			return true
		}
	}
	return false
}

// bhyve reports bhyve availability without loading vmm.
func (d *freebsdDiscoverer) bhyve() models.BhyveInfo {
	info := models.BhyveInfo{VMMLoadable: d.anyExists(vmmModulePaths)}
	if d.exists(bhyveBinary) {
		info.BinaryPath = bhyveBinary
	}
	for _, dev := range []string{vmmCtlDevice, vmmDeviceDir} {
		if d.exists(dev) {
			info.VMMDevice = dev
			break
		}
	}

	loaded, err := d.oidPresent(sysctlVMMLoaded)
	info.VMMLoaded = loaded
	switch {
	case err != nil:
		info.Status = models.StatusUnknown
		info.Reason = "could not determine whether the vmm kernel module is loaded: " + err.Error()
	case info.BinaryPath == "":
		info.Status = models.StatusUnavailable
		info.Reason = "bhyve binary not found at " + bhyveBinary
	case loaded:
		info.Status = models.StatusAvailable
		info.Reason = "vmm kernel module is loaded and " + bhyveBinary + " is present"
	case info.VMMLoadable:
		info.Status = models.StatusUnavailable
		info.Reason = "vmm kernel module is not loaded (vmm.ko is present and can be loaded by an administrator); CPU virtualization support is verified only once vmm is loaded"
	default:
		info.Status = models.StatusUnavailable
		info.Reason = "vmm kernel module is not loaded and vmm.ko was not found"
	}
	return info
}

func (d *freebsdDiscoverer) zfsKernel() zfsKernelState {
	version, err := d.sysctl.String(sysctlZFSVersion)
	switch {
	case err == nil:
		return zfsKernelState{loaded: true, loadable: true, version: strings.TrimSpace(version)}
	case errors.Is(err, ErrSysctlNotFound):
		return zfsKernelState{loadable: d.anyExists(zfsModulePaths)}
	default:
		return zfsKernelState{err: err}
	}
}

// freebsdVersion returns the numeric version of a release string:
// "15.1-RELEASE-p6" becomes "15.1".
func freebsdVersion(release string) string {
	v, _, _ := strings.Cut(release, "-")
	return v
}
