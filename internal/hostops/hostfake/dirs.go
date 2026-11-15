package hostfake

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeDirs simulates the directory pools in memory: nothing on the
// development machine is created. A directory pool has PoolBytes of
// space; a disk that is not thin takes its size.
type fakeDirs struct{ f *Fake }

var _ hostops.Dirs = fakeDirs{}

// Files returns the paths of every simulated disk file, sorted.
func (f *Fake) Files() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.diskFiles))
	for name := range f.diskFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (d fakeDirs) pool(p hostops.DirPool) hostops.Pool {
	f := d.f
	if why := f.UnusableDirs[p.Name]; why != "" {
		return hostops.Pool{Name: p.Name, Kind: hostops.PoolKindDirectory, Error: why}
	}
	var used int64
	for name, file := range f.diskFiles {
		if strings.HasPrefix(name, p.Path+"/") {
			used += file.RefReservationBytes
		}
	}
	return hostops.Pool{Name: p.Name, Kind: hostops.PoolKindDirectory, Health: "ONLINE",
		SizeBytes: f.opts.PoolBytes, AllocatedBytes: used,
		FreeBytes: f.opts.PoolBytes - used, AvailableBytes: f.opts.PoolBytes - used}
}

// usable returns a directory pool that can be used, under the lock.
func (d fakeDirs) usable(name string) (hostops.Pool, error) {
	for _, p := range d.f.conf.Directories {
		if p.Name == name {
			pool := d.pool(p)
			if pool.Error != "" {
				return hostops.Pool{}, &models.Error{Code: models.CodeStorageUnavailable,
					Message: "the directory pool " + name + " cannot be used: " + pool.Error}
			}
			return pool, nil
		}
	}
	_, err := d.f.conf.PoolKind(name)
	if err == nil {
		err = &models.Error{Code: models.CodePoolNotAllowed, Message: "not a directory pool"}
	}
	return hostops.Pool{}, err
}

// Pools implements hostops.Dirs.
func (d fakeDirs) Pools(context.Context) ([]hostops.Pool, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	out := make([]hostops.Pool, 0, len(d.f.conf.Directories))
	for _, p := range d.f.conf.Directories {
		out = append(out, d.pool(p))
	}
	return out, nil
}

// Pool implements hostops.Dirs.
func (d fakeDirs) Pool(_ context.Context, name string) (hostops.Pool, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	return d.usable(name)
}

// CreateVMDir implements hostops.Dirs.
func (d fakeDirs) CreateVMDir(_ context.Context, pool, vmID string) (hostops.Dataset, error) {
	p, rel, err := d.f.conf.DirVMRel(pool, vmID)
	if err != nil {
		return hostops.Dataset{}, err
	}
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if _, err := d.usable(pool); err != nil {
		return hostops.Dataset{}, err
	}
	name := filepath.Join(p.Path, rel)
	if !d.f.dirs[name] {
		d.f.dirs[name] = true
		d.f.record("mkdir %s", name)
	}
	return hostops.Dataset{Name: name, Type: "directory", VMID: vmID}, nil
}

func (d fakeDirs) file(spec hostops.VolumeSpec) (string, hostops.Pool, error) {
	p, rel, err := d.f.conf.DirDiskRel(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return "", hostops.Pool{}, err
	}
	if !hostops.ValidID(spec.DiskID) {
		return "", hostops.Pool{}, models.Invalid("invalid disk ID")
	}
	pool, err := d.usable(spec.Pool)
	return filepath.Join(p.Path, rel), pool, err
}

func noRoom(pool string) error {
	return &models.Error{Code: models.CodeInsufficientSpace,
		Message: "the directory pool " + pool + " does not have room for this disk"}
}

// CreateFile implements hostops.Dirs.
func (d fakeDirs) CreateFile(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	name, pool, err := d.file(spec)
	if err != nil {
		return hostops.Dataset{}, err
	}
	if !d.f.dirs[filepath.Dir(name)] {
		return hostops.Dataset{}, fmt.Errorf("hostfake: %s does not exist", filepath.Dir(name))
	}
	if _, ok := d.f.diskFiles[name]; ok {
		return hostops.Dataset{}, fmt.Errorf("hostfake: refusing to create %s: it exists", name)
	}
	if spec.SizeBytes <= 0 || spec.SizeBytes%512 != 0 {
		return hostops.Dataset{}, models.Invalid("a disk file's size must be a positive multiple of 512 bytes")
	}
	file := hostops.Dataset{Name: name, Type: hostops.TypeFile, VolSizeBytes: spec.SizeBytes,
		VMID: spec.VMID, DiskID: spec.DiskID}
	if !spec.Thin {
		if spec.SizeBytes > pool.AvailableBytes {
			return hostops.Dataset{}, noRoom(spec.Pool)
		}
		file.RefReservationBytes, file.UsedBytes, file.ReferencedBytes = spec.SizeBytes, spec.SizeBytes, spec.SizeBytes
	}
	d.f.diskFiles[name] = file
	d.f.record("create file %s %d thin=%v", name, spec.SizeBytes, spec.Thin)
	return file, nil
}

// ResizeFile implements hostops.Dirs.
func (d fakeDirs) ResizeFile(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	name, pool, err := d.file(spec)
	if err != nil {
		return hostops.Dataset{}, err
	}
	file, ok := d.f.diskFiles[name]
	if !ok {
		return hostops.Dataset{}, fmt.Errorf("the disk file %s: %w", name, models.ErrNotFound)
	}
	switch {
	case spec.SizeBytes <= file.VolSizeBytes:
		return hostops.Dataset{}, models.Invalid(fmt.Sprintf("a disk only grows: the new size must be larger than %d bytes", file.VolSizeBytes))
	case spec.SizeBytes%512 != 0:
		return hostops.Dataset{}, models.Invalid("the new size must be a multiple of 512 bytes")
	}
	if file.RefReservationBytes > 0 {
		if spec.SizeBytes-file.VolSizeBytes > pool.AvailableBytes {
			return hostops.Dataset{}, noRoom(spec.Pool)
		}
		file.RefReservationBytes, file.UsedBytes, file.ReferencedBytes = spec.SizeBytes, spec.SizeBytes, spec.SizeBytes
	}
	file.VolSizeBytes = spec.SizeBytes
	d.f.diskFiles[name] = file
	d.f.record("grow file %s %d", name, spec.SizeBytes)
	return file, nil
}

// DestroyFile implements hostops.Dirs.
func (d fakeDirs) DestroyFile(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	name, _, err := d.file(spec)
	if err != nil {
		return hostops.Dataset{}, err
	}
	file, ok := d.f.diskFiles[name]
	if !ok {
		return hostops.Dataset{}, fmt.Errorf("the disk file %s: %w", name, models.ErrNotFound)
	}
	delete(d.f.diskFiles, name)
	d.f.record("rm %s", name)
	return file, nil
}

func (d fakeDirs) vmFiles(dir string) []hostops.Dataset {
	var out []hostops.Dataset
	for name, file := range d.f.diskFiles {
		if filepath.Dir(name) == dir {
			out = append(out, file)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Name, out[j].Name
		return len(a) < len(b) || len(a) == len(b) && a < b
	})
	return out
}

// VMFiles implements hostops.Dirs.
func (d fakeDirs) VMFiles(_ context.Context, pool, vmID string) ([]hostops.Dataset, error) {
	p, rel, err := d.f.conf.DirVMRel(pool, vmID)
	if err != nil {
		return nil, err
	}
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if _, err := d.usable(pool); err != nil {
		return nil, err
	}
	return d.vmFiles(filepath.Join(p.Path, rel)), nil
}

// DestroyVM implements hostops.Dirs.
func (d fakeDirs) DestroyVM(_ context.Context, pool, vmID string) ([]hostops.Dataset, error) {
	p, rel, err := d.f.conf.DirVMRel(pool, vmID)
	if err != nil {
		return nil, err
	}
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if _, err := d.usable(pool); err != nil {
		return nil, err
	}
	dir := filepath.Join(p.Path, rel)
	if !d.f.dirs[dir] {
		return nil, nil
	}
	gone := d.vmFiles(dir)
	for _, file := range gone {
		delete(d.f.diskFiles, file.Name)
	}
	delete(d.f.dirs, dir)
	d.f.record("rm -r %s", dir)
	return append(gone, hostops.Dataset{Name: dir, Type: "directory", VMID: vmID}), nil
}
