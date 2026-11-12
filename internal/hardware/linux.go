package hardware

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// This file holds the Linux development-mode discovery logic. Linux is only
// a development and CI environment for Skep, never a production host, so
// bhyve is always reported unavailable here. The logic reads an fs.FS rooted
// at "/" so it can be tested with fixtures; the binding is in new_linux.go.

type linuxDiscoverer struct {
	// root is the filesystem root; paths are relative, such as "proc/meminfo".
	root fs.FS
	arch string
	find pathFinder
	run  CommandRunner
	now  func() time.Time
}

func (d *linuxDiscoverer) Discover(ctx context.Context) (models.Hardware, error) {
	if err := ctx.Err(); err != nil {
		return models.Hardware{}, err
	}
	var w warnings

	osRelease := map[string]string{}
	if b, err := fs.ReadFile(d.root, "etc/os-release"); err != nil {
		w.add(fmt.Sprintf("os-release: %v", err))
	} else {
		osRelease = parseOSRelease(b)
	}
	name := osRelease["NAME"]
	if name == "" {
		name = d.read("proc/sys/kernel/ostype", &w)
	}

	cpu := models.CPUInfo{}
	if b, err := fs.ReadFile(d.root, "proc/cpuinfo"); err != nil {
		w.add(fmt.Sprintf("cpuinfo: %v", err))
	} else {
		cpu = parseCPUInfo(b)
		if cpu.Model == "" {
			w.add("cpuinfo: no CPU model name found")
		}
	}

	mem := models.MemoryInfo{}
	if b, err := fs.ReadFile(d.root, "proc/meminfo"); err != nil {
		w.add(fmt.Sprintf("meminfo: %v", err))
	} else if mem, err = parseMemInfo(b); err != nil {
		w.add(fmt.Sprintf("meminfo: %v", err))
	}

	hw := models.Hardware{
		Platform: models.PlatformLinuxDev,
		Host: models.HostInfo{
			Hostname: d.read("proc/sys/kernel/hostname", &w),
			OS: models.OSInfo{
				Name:    name,
				Version: osRelease["VERSION_ID"],
				Release: d.read("proc/sys/kernel/osrelease", &w),
				Kernel:  d.read("proc/version", &w),
			},
			Architecture: d.arch,
		},
		CPU:    cpu,
		Memory: mem,
		Hypervisors: models.Hypervisors{
			Bhyve: models.BhyveInfo{Capability: models.Capability{
				Status: models.StatusUnavailable,
				Reason: "bhyve exists only on FreeBSD; this is a Linux development host",
			}},
			QEMU: discoverQEMU(ctx, d.find, d.run),
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

func (d *linuxDiscoverer) read(name string, w *warnings) string {
	b, err := fs.ReadFile(d.root, name)
	if err != nil {
		w.add(fmt.Sprintf("%s: %v", name, err))
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (d *linuxDiscoverer) zfsKernel() zfsKernelState {
	if _, err := fs.Stat(d.root, "sys/module/zfs"); err != nil {
		return zfsKernelState{}
	}
	k := zfsKernelState{loaded: true, loadable: true}
	if b, err := fs.ReadFile(d.root, "sys/module/zfs/version"); err == nil {
		k.version = strings.TrimSpace(string(b))
	}
	return k
}

// parseOSRelease parses os-release(5) KEY=value lines.
func parseOSRelease(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// parseCPUInfo parses /proc/cpuinfo. Threads is the number of logical CPUs;
// Cores is the number of distinct (physical id, core id) pairs, falling back
// to the thread count when the kernel does not report topology.
func parseCPUInfo(b []byte) models.CPUInfo {
	var info models.CPUInfo
	cores := map[string]struct{}{}
	physical, haveTopology := "", false

	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // the flags line can be long
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			info.Threads++
			physical = ""
		case "model name":
			if info.Model == "" {
				info.Model = v
			}
		case "physical id":
			physical = v
		case "core id":
			haveTopology = true
			cores[physical+"/"+v] = struct{}{}
		}
	}
	if haveTopology {
		info.Cores = len(cores)
	} else {
		info.Cores = info.Threads
	}
	return info
}

// parseMemInfo parses MemTotal and MemAvailable from /proc/meminfo.
func parseMemInfo(b []byte) (models.MemoryInfo, error) {
	var info models.MemoryInfo
	found := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch k {
		case "MemTotal":
			dst = &info.TotalBytes
		case "MemAvailable":
			dst = &info.AvailableBytes
		default:
			continue
		}
		fields := strings.Fields(v)
		if len(fields) != 2 || fields[1] != "kB" {
			return models.MemoryInfo{}, fmt.Errorf("unexpected %s value %q", k, strings.TrimSpace(v))
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return models.MemoryInfo{}, fmt.Errorf("%s: %w", k, err)
		}
		*dst = kb * 1024
		found++
	}
	if found != 2 {
		return models.MemoryInfo{}, fmt.Errorf("MemTotal or MemAvailable missing")
	}
	return info, nil
}
