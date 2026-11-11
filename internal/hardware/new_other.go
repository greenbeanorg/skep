//go:build !freebsd && !linux

package hardware

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// New returns a fallback Discoverer for operating systems Skep does not
// support. It reports what the Go runtime knows and marks every capability
// as unknown, so the code base still builds and runs elsewhere.
func New(Options) Discoverer { return unsupportedDiscoverer{} }

type unsupportedDiscoverer struct{}

func (unsupportedDiscoverer) Discover(ctx context.Context) (models.Hardware, error) {
	if err := ctx.Err(); err != nil {
		return models.Hardware{}, err
	}
	unknown := models.Capability{
		Status: models.StatusUnknown,
		Reason: "host discovery is not implemented for " + runtime.GOOS,
	}
	hostname, _ := os.Hostname()
	return models.Hardware{
		Platform: models.PlatformUnsupported,
		Host: models.HostInfo{
			Hostname:     hostname,
			OS:           models.OSInfo{Name: runtime.GOOS},
			Architecture: runtime.GOARCH,
		},
		CPU: models.CPUInfo{Cores: runtime.NumCPU(), Threads: runtime.NumCPU()},
		Hypervisors: models.Hypervisors{
			Bhyve: models.BhyveInfo{Capability: unknown},
			QEMU:  models.QEMUInfo{Capability: unknown, Binaries: []models.QEMUBinary{}},
		},
		Storage: models.StorageInfo{
			ZFS: models.ZFSInfo{Capability: unknown, Pools: []models.ZFSPool{}},
		},
		Warnings:    []string{"host discovery is not implemented for " + runtime.GOOS},
		CollectedAt: time.Now().UTC(),
	}, nil
}
