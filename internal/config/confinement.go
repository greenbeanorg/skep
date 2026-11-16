package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Host confinement defaults (ADR 0009 decision 1c).
const (
	// DefaultFirmwareDir is where the edk2-bhyve package installs the
	// UEFI firmware.
	DefaultFirmwareDir = "/usr/local/share/edk2-bhyve"
	// DefaultHostMemoryReserve is the memory left to the host when Skep
	// decides whether a VM may start.
	DefaultHostMemoryReserve = 1 << 30
	// DefaultDatasetRoot is the dataset under an allow-listed pool that
	// holds everything Skep creates.
	DefaultDatasetRoot = "skep"
	// MediaDirName is the install media directory, in the data directory.
	MediaDirName = "media"
	// DefaultWatchInterval is how often the agent reads the state of the
	// running VMs.
	DefaultWatchInterval = 2 * time.Second
	// VMRunDirName is the directory of per-VM runtime state, next to the
	// agent socket. It is gone after a reboot.
	VMRunDirName = "vm"
	// VMStateDirName is the directory of per-VM persistent state, next to
	// the agent's database.
	VMStateDirName = "vm"
	// DefaultMaxSnapshotsPerResource and MaxSnapshotsPerResource bound
	// storage.max_snapshots_per_resource.
	DefaultMaxSnapshotsPerResource = 64
	MaxSnapshotsPerResource        = 1000
	// DefaultCapacityWarnPercent and DefaultCapacityFailPercent are the
	// thresholds of the storage_capacity health check (ADR 0010
	// decision 15).
	DefaultCapacityWarnPercent = 80
	DefaultCapacityFailPercent = 90
)

// HypervisorConfig configures the hypervisor engines.
type HypervisorConfig struct {
	Bhyve BhyveConfig `json:"bhyve"`
}

// BhyveConfig configures the bhyve engine.
type BhyveConfig struct {
	// FirmwareDir holds BHYVE_UEFI.fd and BHYVE_UEFI_VARS.fd.
	FirmwareDir string `json:"firmware_dir"`
	// HostMemoryReserveBytes is memory Skep leaves to the host: a VM is
	// not started when the running VMs and it would need more than
	// physical memory minus this. Unset means DefaultHostMemoryReserve.
	HostMemoryReserveBytes *int64 `json:"host_memory_reserve_bytes"`
	// MaxVCPUsPerVM bounds a VM's CPU count. 0 means the lower of the
	// host's logical CPUs and hw.vmm.maxcpu.
	MaxVCPUsPerVM int `json:"max_vcpus_per_vm"`
	// WatchInterval is how often the agent reads the state of the VMs.
	WatchInterval Duration `json:"watch_interval"`
}

// StorageConfig says which storage Skep may use.
type StorageConfig struct {
	// Pools is the allow-list of ZFS pools. A request naming any other
	// pool is refused. Empty, the default, means no VM storage at all.
	Pools []string `json:"pools"`
	// DatasetRoot is the dataset under each pool that holds what Skep
	// creates. Skep never touches a dataset outside it.
	DatasetRoot string `json:"dataset_root"`
	// MediaDir holds the ISO files an operator placed on the host. Skep
	// only reads it.
	MediaDir string `json:"media_dir"`
	// MaxSnapshotsPerResource bounds the snapshots of one VM or jail
	// (ADR 0010 decision 2d). 0 means the default, 64.
	MaxSnapshotsPerResource int `json:"max_snapshots_per_resource"`
	// Directories is the allow-list of directory pools (ADR 0010
	// decision 6): storage for hosts without ZFS. Their names share one
	// namespace with Pools.
	Directories []DirectoryPool `json:"directories"`
	// CapacityWarnPercent and CapacityFailPercent are the thresholds of
	// the storage_capacity health check, in percent of a pool in use
	// (ADR 0010 decision 15). 0 means the defaults, 80 and 90.
	CapacityWarnPercent int `json:"capacity_warn_percent"`
	CapacityFailPercent int `json:"capacity_fail_percent"`
	// TemplatePackages are the base packages a pkgbase jail template
	// installs (ADR 0010 decision 7a). Never taken from a request. Unset
	// means FreeBSD-set-minimal; an empty list means no pkgbase source.
	TemplatePackages []string `json:"template_packages"`
	// DistDir is the root-owned directory where the operator places
	// base.txz and its MANIFEST for dist templates. Empty means no dist
	// source.
	DistDir string `json:"dist_dir"`
}

// DefaultTemplatePackage is what a pkgbase template installs when
// storage.template_packages is not set.
const DefaultTemplatePackage = "FreeBSD-set-minimal"

// templatePackageRE is the shape of a base package name (ADR 0010
// decision 7a).
var templatePackageRE = regexp.MustCompile(`^FreeBSD-[a-z0-9][a-z0-9-]*$`)

// DirectoryPool is one allow-listed directory that holds VM disks as
// files. The directory must exist, be owned by root with mode 0700, and
// be on a local UFS or ZFS file system; the agent checks that each time
// it uses it, not at load.
type DirectoryPool struct {
	// Name is how requests refer to the pool, as they name a ZFS pool.
	Name string `json:"name"`
	// Path is the absolute, clean path of the directory.
	Path string `json:"path"`
}

// NetworkConfig says which network interfaces Skep may use.
type NetworkConfig struct {
	// Bridges is the allow-list of bridges a VM's NIC may join. Skep
	// creates none of them.
	Bridges []string `json:"bridges"`
	// ManagementInterfaces can never be a member of a bridge Skep puts a
	// guest on.
	ManagementInterfaces []string `json:"management_interfaces"`
}

var (
	// poolNameRE is the shape of a ZFS pool name Skep accepts.
	poolNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	// datasetComponentRE is one component of a dataset name.
	datasetComponentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// interfaceNameRE is the shape of a network interface name.
	interfaceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,14}$`)
)

// ValidPoolName reports whether s has the shape of a pool name.
func ValidPoolName(s string) bool { return poolNameRE.MatchString(s) }

// ValidInterfaceName reports whether s has the shape of an interface name.
func ValidInterfaceName(s string) bool { return interfaceNameRE.MatchString(s) }

func (c *Config) applyConfinementDefaults() {
	b := &c.Hypervisor.Bhyve
	if b.FirmwareDir == "" {
		b.FirmwareDir = DefaultFirmwareDir
	}
	if b.HostMemoryReserveBytes == nil {
		reserve := int64(DefaultHostMemoryReserve)
		b.HostMemoryReserveBytes = &reserve
	}
	if b.WatchInterval == 0 {
		b.WatchInterval = Duration(DefaultWatchInterval)
	}
	if c.Storage.DatasetRoot == "" {
		c.Storage.DatasetRoot = DefaultDatasetRoot
	}
	if c.Storage.MediaDir == "" {
		c.Storage.MediaDir = filepath.Join(c.DataDir, MediaDirName)
	}
	if c.Storage.Pools == nil {
		c.Storage.Pools = []string{}
	}
	if c.Storage.Directories == nil {
		c.Storage.Directories = []DirectoryPool{}
	}
	if c.Storage.TemplatePackages == nil {
		c.Storage.TemplatePackages = []string{DefaultTemplatePackage}
	}
	if c.Storage.MaxSnapshotsPerResource == 0 {
		c.Storage.MaxSnapshotsPerResource = DefaultMaxSnapshotsPerResource
	}
	if c.Storage.CapacityWarnPercent == 0 {
		c.Storage.CapacityWarnPercent = DefaultCapacityWarnPercent
	}
	if c.Storage.CapacityFailPercent == 0 {
		c.Storage.CapacityFailPercent = DefaultCapacityFailPercent
	}
	if c.Network.Bridges == nil {
		c.Network.Bridges = []string{}
	}
	if c.Network.ManagementInterfaces == nil {
		c.Network.ManagementInterfaces = []string{}
	}
}

func (c Config) validateConfinement() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	b := c.Hypervisor.Bhyve
	if !c.DevMode && !filepath.IsAbs(b.FirmwareDir) {
		add("hypervisor.bhyve.firmware_dir %q must be an absolute path", b.FirmwareDir)
	}
	if b.HostMemoryReserveBytes != nil && *b.HostMemoryReserveBytes < 0 {
		add("hypervisor.bhyve.host_memory_reserve_bytes must not be negative")
	}
	if b.MaxVCPUsPerVM < 0 || b.MaxVCPUsPerVM > 1024 {
		add("hypervisor.bhyve.max_vcpus_per_vm must be between 0 and 1024")
	}
	if w := b.WatchInterval.Std(); w < 200*time.Millisecond || w > time.Minute {
		add("hypervisor.bhyve.watch_interval must be between 200ms and 1m")
	}

	seen := map[string]bool{}
	for _, p := range c.Storage.Pools {
		switch {
		case !poolNameRE.MatchString(p):
			add("storage.pools: %q is not a pool name", p)
		case seen[p]:
			add("storage.pools: %q is listed twice", p)
		}
		seen[p] = true
	}
	paths := map[string]bool{}
	for _, d := range c.Storage.Directories {
		switch {
		case !poolNameRE.MatchString(d.Name):
			add("storage.directories: %q is not a pool name", d.Name)
		case seen[d.Name]:
			add("storage.directories: the name %q is already used by another pool", d.Name)
		case !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path || d.Path == "/":
			add("storage.directories: the path %q of %q must be an absolute, clean path other than /", d.Path, d.Name)
		case paths[d.Path]:
			add("storage.directories: the path %q is listed twice", d.Path)
		}
		seen[d.Name] = true
		paths[d.Path] = true
	}
	for i, a := range c.Storage.Directories {
		for j, b := range c.Storage.Directories {
			if i != j && a.Path != b.Path && strings.HasPrefix(b.Path, a.Path+"/") {
				add("storage.directories: the path of %q is inside the path of %q", b.Name, a.Name)
			}
		}
	}
	for _, part := range strings.Split(c.Storage.DatasetRoot, "/") {
		if !datasetComponentRE.MatchString(part) {
			add("storage.dataset_root %q must be a dataset name relative to the pool, such as \"skep\"", c.Storage.DatasetRoot)
			break
		}
	}
	if n := c.Storage.MaxSnapshotsPerResource; n < 1 || n > MaxSnapshotsPerResource {
		add("storage.max_snapshots_per_resource must be between 1 and %d", MaxSnapshotsPerResource)
	}
	if w, f := c.Storage.CapacityWarnPercent, c.Storage.CapacityFailPercent; w < 1 || f > 100 || w >= f {
		add("storage.capacity_warn_percent (%d) and storage.capacity_fail_percent (%d) must satisfy 1 <= warn < fail <= 100", w, f)
	}
	pkgs := map[string]bool{}
	for _, p := range c.Storage.TemplatePackages {
		switch {
		case !templatePackageRE.MatchString(p):
			add("storage.template_packages: %q is not a base package name (FreeBSD-...)", p)
		case pkgs[p]:
			add("storage.template_packages: %q is listed twice", p)
		}
		pkgs[p] = true
	}
	if d := c.Storage.DistDir; d != "" && (!filepath.IsAbs(d) || filepath.Clean(d) != d || d == "/") {
		add("storage.dist_dir %q must be an absolute, clean path other than /", d)
	}
	if !c.DevMode && !filepath.IsAbs(c.Storage.MediaDir) {
		add("storage.media_dir %q must be an absolute path", c.Storage.MediaDir)
	}

	management := map[string]bool{}
	for _, n := range c.Network.ManagementInterfaces {
		if !interfaceNameRE.MatchString(n) {
			add("network.management_interfaces: %q is not an interface name", n)
		}
		management[n] = true
	}
	seen = map[string]bool{}
	for _, n := range c.Network.Bridges {
		switch {
		case !interfaceNameRE.MatchString(n):
			add("network.bridges: %q is not an interface name", n)
		case seen[n]:
			add("network.bridges: %q is listed twice", n)
		case management[n]:
			add("network.bridges: %q is a management interface and can never carry guests", n)
		}
		seen[n] = true
	}
	return errs
}

// VMRunDir is the directory of per-VM runtime state: next to the agent
// socket, so under /var/run/skep in production.
func (c Config) VMRunDir() string {
	return filepath.Join(filepath.Dir(c.Socket.Path), VMRunDirName)
}

// VMStateDir is the directory of per-VM persistent state: next to the
// agent's database, in the directory only root can open.
func (c Config) VMStateDir() string {
	return filepath.Join(filepath.Dir(c.Database.Path), VMStateDirName)
}
