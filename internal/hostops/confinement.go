package hostops

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Confinement says what Skep may touch on this host. It is built from the
// agent's root-owned configuration and never from a request.
type Confinement struct {
	// Pools is the allow-list of ZFS pools.
	Pools []string
	// Directories is the allow-list of directory pools (ADR 0010
	// decision 6). Their names never repeat a name in Pools.
	Directories []DirPool
	// DatasetRoot is the dataset under each pool that holds everything
	// Skep creates, for example "skep".
	DatasetRoot string
	// MediaDir is the install media directory. Skep only reads it.
	MediaDir string
	// Bridges is the allow-list of bridges a NIC may join.
	Bridges []string
	// ManagementInterfaces may never be a member of a guest bridge.
	ManagementInterfaces []string
	// FirmwareDir holds the UEFI firmware files.
	FirmwareDir string
	// RunDir holds one runtime directory per VM; it is gone after a
	// reboot. StateDir holds one persistent directory per VM.
	RunDir   string
	StateDir string
	// DistDir is storage.dist_dir: the root-owned directory where the
	// operator places base.txz and its MANIFEST for dist templates. Empty
	// means there is no dist source.
	DistDir string
}

// DirPool is an allow-listed directory that holds VM disks as files.
type DirPool struct {
	Name string
	// Path is absolute and clean.
	Path string
}

// Pool kinds.
const (
	PoolKindZFS       = "zfs"
	PoolKindDirectory = "directory"
)

// Fixed names of the firmware files in FirmwareDir (the edk2-bhyve
// package) and of the per-VM files.
const (
	FirmwareCodeFile = "BHYVE_UEFI.fd"
	FirmwareVarsFile = "BHYVE_UEFI_VARS.fd"
	VMVarsFile       = "uefi-vars.fd"
	// InstancePrefix starts the vmm instance name of every Skep VM.
	InstancePrefix = "skep-"
)

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// mediaNameRE is the shape of an ISO file name (ADR 0009 decision 4).
	// It cannot contain a path separator.
	mediaNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.iso$`)
)

// ValidID reports whether s is a resource ID as Skep generates them.
func ValidID(s string) bool { return uuidRE.MatchString(s) }

// ValidMediaName reports whether s has the shape of an ISO file name.
func ValidMediaName(s string) bool { return mediaNameRE.MatchString(s) }

// MaxSlot bounds a disk or NIC slot number.
const MaxSlot = 31

func invalid(format string, args ...any) error {
	return models.Invalid(fmt.Sprintf(format, args...))
}

func checkVMID(vmID string) error {
	if !ValidID(vmID) {
		return invalid("invalid VM ID")
	}
	return nil
}

// CheckPool refuses a pool that is not on the allow-list.
func (c Confinement) CheckPool(pool string) error {
	for _, p := range c.Pools {
		if p == pool {
			return nil
		}
	}
	return &models.Error{
		Code:    models.CodePoolNotAllowed,
		Message: "the pool is not in storage.pools of the agent's configuration",
		Details: map[string]any{"pool": printable(pool)},
	}
}

// PoolKind says whether an allow-listed pool is a ZFS pool or a
// directory pool, and refuses a name that is neither.
func (c Confinement) PoolKind(pool string) (string, error) {
	for _, p := range c.Pools {
		if p == pool {
			return PoolKindZFS, nil
		}
	}
	for _, d := range c.Directories {
		if d.Name == pool {
			return PoolKindDirectory, nil
		}
	}
	return "", &models.Error{
		Code:    models.CodePoolNotAllowed,
		Message: "the pool is not in storage.pools or storage.directories of the agent's configuration",
		Details: map[string]any{"pool": printable(pool)},
	}
}

// IsDirectory reports whether pool is an allow-listed directory pool.
func (c Confinement) IsDirectory(pool string) bool {
	kind, err := c.PoolKind(pool)
	return err == nil && kind == PoolKindDirectory
}

// dirPool returns an allow-listed directory pool.
func (c Confinement) dirPool(pool string) (DirPool, error) {
	for _, d := range c.Directories {
		if d.Name == pool {
			return d, nil
		}
	}
	return DirPool{}, &models.Error{
		Code:    models.CodePoolNotAllowed,
		Message: "the pool is not in storage.directories of the agent's configuration",
		Details: map[string]any{"pool": printable(pool)},
	}
}

// DirVMRel is the directory of a VM in a directory pool, relative to the
// pool's path: <root>/vm/<vm-id>.
func (c Confinement) DirVMRel(pool, vmID string) (DirPool, string, error) {
	d, err := c.dirPool(pool)
	if err != nil {
		return DirPool{}, "", err
	}
	if c.DatasetRoot == "" {
		return DirPool{}, "", fmt.Errorf("hostops: the dataset root is not configured")
	}
	if err := checkVMID(vmID); err != nil {
		return DirPool{}, "", err
	}
	return d, c.DatasetRoot + "/vm/" + vmID, nil
}

// DiskFileName is the file name of a disk in a VM's directory.
func DiskFileName(slot int) (string, error) {
	if slot < 0 || slot > MaxSlot {
		return "", invalid("invalid disk slot")
	}
	return "disk" + strconv.Itoa(slot) + ".img", nil
}

// DirDiskRel is the file of a disk in a directory pool, relative to the
// pool's path: <root>/vm/<vm-id>/disk<slot>.img.
func (c Confinement) DirDiskRel(pool, vmID string, slot int) (DirPool, string, error) {
	d, dir, err := c.DirVMRel(pool, vmID)
	if err != nil {
		return DirPool{}, "", err
	}
	name, err := DiskFileName(slot)
	if err != nil {
		return DirPool{}, "", err
	}
	return d, dir + "/" + name, nil
}

// DiskPath is what bhyve opens for a disk: the zvol's device node in a
// ZFS pool, the disk's file in a directory pool.
func (c Confinement) DiskPath(pool, vmID string, slot int) (string, error) {
	kind, err := c.PoolKind(pool)
	if err != nil {
		return "", err
	}
	if kind == PoolKindZFS {
		return c.DiskDevice(pool, vmID, slot)
	}
	d, rel, err := c.DirDiskRel(pool, vmID, slot)
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Path, rel), nil
}

// CheckBridge refuses a bridge that is not on the allow-list.
func (c Confinement) CheckBridge(bridge string) error {
	for _, b := range c.Bridges {
		if b == bridge {
			return nil
		}
	}
	return &models.Error{
		Code:    models.CodeBridgeNotAllowed,
		Message: "the bridge is not in network.bridges of the agent's configuration",
		Details: map[string]any{"bridge": printable(bridge)},
	}
}

// IsManagement reports whether an interface is a management interface.
func (c Confinement) IsManagement(name string) bool {
	for _, m := range c.ManagementInterfaces {
		if m == name {
			return true
		}
	}
	return false
}

// printable bounds and cleans a rejected name before it is echoed in an
// error: what was refused is by definition not validated.
func printable(s string) string {
	out := make([]rune, 0, 64)
	for _, r := range s {
		if len(out) == 64 {
			break
		}
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}

// rootDataset is <pool>/<root>, the top of what Skep owns in a pool.
func (c Confinement) rootDataset(pool string) (string, error) {
	if err := c.CheckPool(pool); err != nil {
		return "", err
	}
	if c.DatasetRoot == "" {
		return "", fmt.Errorf("hostops: the dataset root is not configured")
	}
	return pool + "/" + c.DatasetRoot, nil
}

// VMDataset is the dataset of a VM: <pool>/<root>/vm/<vm-id>.
func (c Confinement) VMDataset(pool, vmID string) (string, error) {
	root, err := c.rootDataset(pool)
	if err != nil {
		return "", err
	}
	if err := checkVMID(vmID); err != nil {
		return "", err
	}
	return root + "/vm/" + vmID, nil
}

// DiskDataset is the zvol of a disk: <vm dataset>/disk<slot>.
func (c Confinement) DiskDataset(pool, vmID string, slot int) (string, error) {
	ds, err := c.VMDataset(pool, vmID)
	if err != nil {
		return "", err
	}
	if slot < 0 || slot > MaxSlot {
		return "", invalid("invalid disk slot")
	}
	return ds + "/disk" + strconv.Itoa(slot), nil
}

// DiskDevice is the device node of a disk's zvol.
func (c Confinement) DiskDevice(pool, vmID string, slot int) (string, error) {
	ds, err := c.DiskDataset(pool, vmID, slot)
	if err != nil {
		return "", err
	}
	return "/dev/zvol/" + ds, nil
}

// Instance is the vmm instance name of a VM. It is also the name of its
// nmdm pair.
func Instance(vmID string) (string, error) {
	if err := checkVMID(vmID); err != nil {
		return "", err
	}
	return InstancePrefix + vmID, nil
}

// VMStateDir is the persistent directory of a VM.
func (c Confinement) VMStateDir(vmID string) (string, error) {
	if err := checkVMID(vmID); err != nil {
		return "", err
	}
	return filepath.Join(c.StateDir, vmID), nil
}

// VMRunDir is the runtime directory of a VM.
func (c Confinement) VMRunDir(vmID string) (string, error) {
	if err := checkVMID(vmID); err != nil {
		return "", err
	}
	return filepath.Join(c.RunDir, vmID), nil
}

// VMVarsPath is the UEFI variable store of a VM.
func (c Confinement) VMVarsPath(vmID string) (string, error) {
	dir, err := c.VMStateDir(vmID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, VMVarsFile), nil
}

// Owner kinds: what a dataset under <pool>/<root> belongs to (ADR 0010
// decision 1).
const (
	OwnerVM        = "vm"
	OwnerContainer = "container"
)

// Owner is the resource a tree of datasets belongs to: a VM or a jail.
type Owner struct {
	Kind string
	ID   string
}

// ownerPlaces maps an owner kind to the dataset under <root> that holds
// one dataset per resource, and to the user property naming the owner.
var ownerPlaces = map[string]struct{ dir, prop string }{
	OwnerVM:        {"vm", propVMID},
	OwnerContainer: {"jail", propContainerID},
}

// OwnerDataset is the dataset of a resource: <pool>/<root>/vm/<id> for a
// VM, <pool>/<root>/jail/<id> for a jail.
func (c Confinement) OwnerDataset(pool string, o Owner) (string, error) {
	place, ok := ownerPlaces[o.Kind]
	if !ok {
		return "", invalid("invalid owner kind")
	}
	root, err := c.rootDataset(pool)
	if err != nil {
		return "", err
	}
	if !ValidID(o.ID) {
		return "", invalid("invalid %s ID", o.Kind)
	}
	return root + "/" + place.dir + "/" + o.ID, nil
}

// SnapshotPrefix starts the ZFS name of every snapshot Skep takes.
const SnapshotPrefix = "skep-"

// SnapshotName is the ZFS short name of a snapshot: skep-<snapshot-id>.
// The user's label is never part of it.
func SnapshotName(snapshotID string) (string, error) {
	if !ValidID(snapshotID) {
		return "", invalid("invalid snapshot ID")
	}
	return SnapshotPrefix + snapshotID, nil
}

// RootDataset is <pool>/<root>, the top of what Skep owns in a pool.
func (c Confinement) RootDataset(pool string) (string, error) { return c.rootDataset(pool) }
