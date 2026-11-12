package hardware

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// zpoolListArgs is the fixed, read-only zpool(8) invocation used to list
// pools: -H for tab-separated output without headers, -p for exact numbers.
var zpoolListArgs = []string{"list", "-H", "-p", "-o", "name,size,allocated,free,fragmentation,capacity,health"}

// parseZpoolList parses the output of "zpool list -H -p -o
// name,size,allocated,free,fragmentation,capacity,health".
func parseZpoolList(out []byte) ([]models.ZFSPool, error) {
	pools := []models.ZFSPool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		f := strings.Split(text, "\t")
		if len(f) != 7 {
			return nil, fmt.Errorf("zpool list line %d: want 7 tab-separated fields, got %d", line, len(f))
		}
		p := models.ZFSPool{Name: f[0], Health: f[6]}
		var err error
		if p.SizeBytes, err = strconv.ParseUint(f[1], 10, 64); err != nil {
			return nil, fmt.Errorf("zpool list line %d: size %q: %w", line, f[1], err)
		}
		if p.AllocatedBytes, err = strconv.ParseUint(f[2], 10, 64); err != nil {
			return nil, fmt.Errorf("zpool list line %d: allocated %q: %w", line, f[2], err)
		}
		if p.FreeBytes, err = strconv.ParseUint(f[3], 10, 64); err != nil {
			return nil, fmt.Errorf("zpool list line %d: free %q: %w", line, f[3], err)
		}
		if p.FragmentationPercent, err = parsePercent(f[4]); err != nil {
			return nil, fmt.Errorf("zpool list line %d: fragmentation: %w", line, err)
		}
		if p.CapacityPercent, err = parsePercent(f[5]); err != nil {
			return nil, fmt.Errorf("zpool list line %d: capacity: %w", line, err)
		}
		pools = append(pools, p)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("zpool list: %w", err)
	}
	return pools, nil
}

// parsePercent parses "12", "12%", or "-" (not reported, returned as nil).
func parsePercent(s string) (*int, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	if s == "-" || s == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid percentage %q", s)
	}
	return &n, nil
}

// zfsKernelState is what the platform knows about the ZFS kernel module.
type zfsKernelState struct {
	loaded   bool
	loadable bool
	version  string
	// err is set when the module state could not be determined.
	err error
}

// discoverZFS reports ZFS availability and lists imported pools.
//
// The pool list is only requested when the kernel module is already loaded:
// on FreeBSD, zpool(8) run as root loads zfs.ko on demand, and discovery
// must never change host state.
func discoverZFS(ctx context.Context, k zfsKernelState, find pathFinder, run CommandRunner) models.ZFSInfo {
	info := models.ZFSInfo{
		ModuleLoaded:  k.loaded,
		ModuleVersion: k.version,
		Pools:         []models.ZFSPool{},
	}
	zpool, haveZpool := find("zpool")
	if haveZpool {
		info.ZpoolPath = zpool
	}

	switch {
	case k.err != nil:
		info.Status = models.StatusUnknown
		info.Reason = "could not determine whether the zfs kernel module is loaded: " + k.err.Error()
		return info
	case !k.loaded && k.loadable:
		info.Status = models.StatusUnavailable
		info.Reason = "zfs kernel module is not loaded (zfs.ko is present and can be loaded by an administrator); pools were not queried"
		return info
	case !k.loaded:
		info.Status = models.StatusUnavailable
		info.Reason = "zfs kernel module is not loaded and was not found"
		return info
	case !haveZpool:
		info.Status = models.StatusUnavailable
		info.Reason = "zfs kernel module is loaded but the zpool binary was not found"
		return info
	}

	info.Status = models.StatusAvailable
	out, err := run.Run(ctx, zpool, zpoolListArgs...)
	if err == nil {
		info.Pools, err = parseZpoolList(out)
	}
	if err != nil {
		info.Pools = []models.ZFSPool{}
		info.PoolsError = err.Error()
		info.Reason = "zfs kernel module is loaded, but the pool list could not be read"
		return info
	}
	info.Reason = fmt.Sprintf("zfs kernel module is loaded; %d pool(s) imported", len(info.Pools))
	return info
}
