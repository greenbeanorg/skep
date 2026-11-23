package hostops

import (
	"context"
	"errors"
)

// Disks routes the disk operations of a VM to the kind of its pool: zvols
// in a ZFS pool, files in a directory pool (ADR 0010 decision 6). The
// hypervisor adapter and the VM service use it for everything a disk
// needs in either kind; snapshots and clones stay with ZFS, and callers
// refuse them on directory pools first.
type Disks struct {
	conf Confinement
	zfs  ZFS
	dirs Dirs
}

// Disks returns the router over o.
func (o Ops) Disks() Disks { return Disks{conf: o.Conf, zfs: o.ZFS, dirs: o.Dirs} }

var errNoDirs = errors.New("hostops: directory pools are not available in this implementation")

// kind returns the kind of an allow-listed pool, checking that its
// implementation exists.
func (d Disks) kind(pool string) (string, error) {
	kind, err := d.conf.PoolKind(pool)
	if err != nil {
		return "", err
	}
	if kind == PoolKindDirectory && d.dirs == nil {
		return "", errNoDirs
	}
	return kind, nil
}

// Pools reports every allow-listed pool, the ZFS pools first, in the order
// of the configuration.
func (d Disks) Pools(ctx context.Context) ([]Pool, error) {
	var out []Pool
	if len(d.conf.Pools) > 0 {
		zp, err := d.zfs.Pools(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range zp {
			p.Kind = PoolKindZFS
			out = append(out, p)
		}
	}
	if len(d.conf.Directories) > 0 {
		if d.dirs == nil {
			return nil, errNoDirs
		}
		dp, err := d.dirs.Pools(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, dp...)
	}
	return out, nil
}

// Pool reports one allow-listed pool of either kind.
func (d Disks) Pool(ctx context.Context, name string) (Pool, error) {
	kind, err := d.kind(name)
	if err != nil {
		return Pool{}, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.Pool(ctx, name)
	}
	p, err := d.zfs.Pool(ctx, name)
	p.Kind = PoolKindZFS
	return p, err
}

// PrepareVM creates the place of a VM's disks in a pool: its dataset, or
// its directory.
func (d Disks) PrepareVM(ctx context.Context, pool, vmID string) (Dataset, error) {
	kind, err := d.kind(pool)
	if err != nil {
		return Dataset{}, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.CreateVMDir(ctx, pool, vmID)
	}
	return d.zfs.CreateVMDataset(ctx, pool, vmID)
}

// Create creates a disk: a zvol, or a file.
func (d Disks) Create(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	kind, err := d.kind(spec.Pool)
	if err != nil {
		return Dataset{}, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.CreateFile(ctx, spec)
	}
	return d.zfs.CreateVolume(ctx, spec)
}

// EnsureDevice makes sure what bhyve opens for a disk exists: the zvol's
// device node in a ZFS pool (ZFS.EnsureVolumeDevice). A file in a
// directory pool has nothing to ensure.
func (d Disks) EnsureDevice(ctx context.Context, spec VolumeSpec, stopped StoppedCheck) (DeviceResult, error) {
	kind, err := d.kind(spec.Pool)
	if err != nil {
		return DeviceResult{}, err
	}
	if kind == PoolKindDirectory {
		return DeviceResult{}, nil
	}
	return d.zfs.EnsureVolumeDevice(ctx, spec, stopped)
}

// Resize grows a disk. It never shrinks one.
func (d Disks) Resize(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	kind, err := d.kind(spec.Pool)
	if err != nil {
		return Dataset{}, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.ResizeFile(ctx, spec)
	}
	return d.zfs.ResizeVolume(ctx, spec)
}

// Destroy destroys one disk.
func (d Disks) Destroy(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	kind, err := d.kind(spec.Pool)
	if err != nil {
		return Dataset{}, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.DestroyFile(ctx, spec)
	}
	return d.zfs.DestroyVolume(ctx, spec)
}

// DestroyVM destroys every disk of a VM in a pool, and the place that
// held them.
func (d Disks) DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	kind, err := d.kind(pool)
	if err != nil {
		return nil, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.DestroyVM(ctx, pool, vmID)
	}
	return d.zfs.DestroyVM(ctx, pool, vmID)
}

// VMVolumes lists what a VM has in a pool: its datasets, or its disk
// files.
func (d Disks) VMVolumes(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	kind, err := d.kind(pool)
	if err != nil {
		return nil, err
	}
	if kind == PoolKindDirectory {
		return d.dirs.VMFiles(ctx, pool, vmID)
	}
	return d.zfs.VMDatasets(ctx, pool, vmID)
}
