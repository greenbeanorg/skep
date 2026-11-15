package hostops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Directory pools (ADR 0010 decision 6): storage for hosts without ZFS. A
// directory pool is an allow-listed directory, owned by root with mode
// 0700, on a local UFS or ZFS file system. A VM's disks are sparse raw
// files <dir>/<root>/vm/<vm-id>/disk<slot>.img that bhyve opens with its
// file backend. There are no snapshots, clones, or quotas.
//
// Every path is derived from the configuration and IDs, and every file
// operation goes through an os.Root opened on the pool's directory, so no
// name can resolve outside it, even through a symbolic link. The pool's
// own directory is checked before it is opened: a directory that is a
// symbolic link, not owned by root, open to group or others, or on a file
// system other than UFS or ZFS is not used.

// Dirs keeps the disk files of VMs in the allow-listed directory pools.
type Dirs interface {
	// Pools reports every directory pool. A pool that cannot be used is
	// reported with an Error, not left out.
	Pools(ctx context.Context) ([]Pool, error)
	// Pool reports one directory pool, or an error when it cannot be
	// used: pool_not_allowed, storage_unavailable.
	Pool(ctx context.Context, name string) (Pool, error)
	// CreateVMDir creates <dir>/<root>/vm/<vm-id>, and the directories
	// above it on first use, each 0700.
	CreateVMDir(ctx context.Context, pool, vmID string) (Dataset, error)
	// CreateFile creates the file of a disk: sparse with its size, or,
	// for a disk that is not thin, written with zeros so its blocks are
	// allocated. It never replaces a file that exists.
	CreateFile(ctx context.Context, spec VolumeSpec) (Dataset, error)
	// ResizeFile grows the file of a disk. It never shrinks one. The
	// growth of a disk that is not thin is written with zeros.
	ResizeFile(ctx context.Context, spec VolumeSpec) (Dataset, error)
	// DestroyFile removes the file of a disk.
	DestroyFile(ctx context.Context, spec VolumeSpec) (Dataset, error)
	// DestroyVM removes the disk files of a VM and its directory, and
	// returns what existed. It refuses a directory holding anything but
	// disk files. A VM without a directory yields an empty list.
	DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error)
	// VMFiles lists the disk files of a VM.
	VMFiles(ctx context.Context, pool, vmID string) ([]Dataset, error)
}

// DirStat is what a directory pool's checks read about its directory:
// lstat(2) and statfs(2).
type DirStat struct {
	// Mode has the type bits of lstat(2): a symbolic link is not a
	// directory.
	Mode fs.FileMode
	UID  uint32
	// Dev and Ino identify the directory, so the one opened can be
	// compared with the one checked.
	Dev    uint64
	Ino    uint64
	FSType string
	// Sizes of the file system: total, free, and available to a
	// non-root user (what a new file can count on).
	SizeBytes      int64
	FreeBytes      int64
	AvailableBytes int64
}

// dirStatter reads a DirStat. The FreeBSD binding is in sys_freebsd.go.
type dirStatter interface {
	StatDir(path string) (DirStat, error)
}

// Dataset type of a disk file.
const TypeFile = "file"

// dirFSTypes are the file systems a directory pool may be on: local ones
// with sparse files. NFS, tmpfs, and the rest are refused.
var dirFSTypes = map[string]bool{"ufs": true, "zfs": true}

// diskFileRE is the shape of a disk file in a VM's directory.
var diskFileRE = regexp.MustCompile(`^disk([0-9]|[12][0-9]|3[01])\.img$`)

// zeroChunk is how much a preallocation writes between cancellation
// checks.
const zeroChunk = 1 << 20

// dirs is the real implementation of Dirs.
type dirs struct {
	conf Confinement
	stat dirStatter
	log  *slog.Logger
}

// newDirs returns the Dirs implementation over the given statter.
func newDirs(conf Confinement, stat dirStatter, log *slog.Logger) *dirs {
	if log == nil {
		log = slog.Default()
	}
	return &dirs{conf: conf, stat: stat, log: log}
}

func unavailable(pool, why string) error {
	return &models.Error{Code: models.CodeStorageUnavailable,
		Message: "the directory pool " + pool + " cannot be used: " + why,
		Details: map[string]any{"pool": pool}}
}

// check reads a directory pool and says why it cannot be used, if it
// cannot.
func (d *dirs) check(p DirPool) (DirStat, string) {
	st, err := d.stat.StatDir(p.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, "the directory " + p.Path + " does not exist"
	case err != nil:
		return st, err.Error()
	case st.Mode&fs.ModeSymlink != 0:
		return st, p.Path + " is a symbolic link; storage.directories must name the directory itself"
	case !st.Mode.IsDir():
		return st, p.Path + " is not a directory"
	case st.UID != 0:
		return st, p.Path + " is not owned by root"
	case st.Mode.Perm()&0o077 != 0:
		return st, fmt.Sprintf("%s has mode %#o; it must be 0700", p.Path, st.Mode.Perm())
	case !dirFSTypes[st.FSType]:
		return st, fmt.Sprintf("%s is on a %s file system; a directory pool must be on a local UFS or ZFS file system", p.Path, st.FSType)
	}
	return st, ""
}

func poolOf(p DirPool, st DirStat, why string) Pool {
	out := Pool{Name: p.Name, Kind: PoolKindDirectory, Error: why}
	if why == "" {
		out.Health = "ONLINE"
		out.SizeBytes, out.FreeBytes, out.AvailableBytes = st.SizeBytes, st.FreeBytes, st.AvailableBytes
		out.AllocatedBytes = max(st.SizeBytes-st.FreeBytes, 0)
	}
	return out
}

// Pools implements Dirs.
func (d *dirs) Pools(ctx context.Context) ([]Pool, error) {
	out := make([]Pool, 0, len(d.conf.Directories))
	for _, p := range d.conf.Directories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, why := d.check(p)
		out = append(out, poolOf(p, st, why))
	}
	return out, nil
}

// Pool implements Dirs.
func (d *dirs) Pool(ctx context.Context, name string) (Pool, error) {
	p, err := d.conf.dirPool(name)
	if err != nil {
		return Pool{}, err
	}
	st, why := d.check(p)
	if why != "" {
		return Pool{}, unavailable(name, why)
	}
	return poolOf(p, st, ""), nil
}

// open checks a directory pool and opens it as an os.Root.
func (d *dirs) open(p DirPool) (*os.Root, DirStat, error) {
	st, why := d.check(p)
	if why != "" {
		return nil, st, unavailable(p.Name, why)
	}
	root, err := os.OpenRoot(p.Path)
	if err != nil {
		return nil, st, unavailable(p.Name, err.Error())
	}
	// OpenRoot follows a symbolic link, so the path could have been
	// swapped since it was checked by whoever can write its parent. The
	// directory opened must be the one checked (same device and inode,
	// whose owner only root can change), and still mode 0700.
	fi, err := root.Stat(".")
	if err == nil {
		id, ok := fileIdentity(fi)
		switch {
		case !ok || id.Dev != st.Dev || id.Ino != st.Ino:
			err = errors.New(p.Path + " changed while it was being opened")
		case fi.Mode().Perm()&0o077 != 0 || !fi.IsDir():
			// The owner was checked on this inode; only root changes it.
			err = errors.New(p.Path + " is no longer a directory with mode 0700")
		}
	}
	if err != nil {
		root.Close()
		return nil, st, unavailable(p.Name, err.Error())
	}
	return root, st, nil
}

// CreateVMDir implements Dirs.
func (d *dirs) CreateVMDir(ctx context.Context, pool, vmID string) (Dataset, error) {
	p, rel, err := d.conf.DirVMRel(pool, vmID)
	if err != nil {
		return Dataset{}, err
	}
	root, _, err := d.open(p)
	if err != nil {
		return Dataset{}, err
	}
	defer root.Close()
	if err := root.MkdirAll(rel, 0o700); err != nil {
		return Dataset{}, fmt.Errorf("hostops: create %s in %s: %w", rel, p.Path, err)
	}
	if fi, err := root.Lstat(rel); err != nil || !fi.IsDir() {
		return Dataset{}, fmt.Errorf("hostops: %s in %s is not a directory", rel, p.Path)
	}
	return Dataset{Name: filepath.Join(p.Path, rel), Type: "directory", VMID: vmID}, nil
}

// diskFile checks a disk's spec and returns its pool, the open root, and
// the file's path relative to it.
func (d *dirs) diskFile(spec VolumeSpec) (DirPool, *os.Root, DirStat, string, error) {
	p, rel, err := d.conf.DirDiskRel(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return DirPool{}, nil, DirStat{}, "", err
	}
	if !ValidID(spec.DiskID) {
		return DirPool{}, nil, DirStat{}, "", invalid("invalid disk ID")
	}
	root, st, err := d.open(p)
	if err != nil {
		return DirPool{}, nil, DirStat{}, "", err
	}
	return p, root, st, rel, nil
}

// regularFile reads a disk file and refuses anything that is not a
// regular file: a symbolic link is never followed.
func regularFile(root *os.Root, rel string) (fs.FileInfo, error) {
	fi, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound("the disk file " + rel)
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("hostops: refusing %s: it is not a regular file", rel)
	}
	return fi, nil
}

func fileDataset(p DirPool, rel string, spec VolumeSpec, fi fs.FileInfo) Dataset {
	return Dataset{
		Name: filepath.Join(p.Path, rel), Type: TypeFile, VolSizeBytes: fi.Size(),
		UsedBytes: allocatedBytes(fi), ReferencedBytes: allocatedBytes(fi),
		VMID: spec.VMID, DiskID: spec.DiskID,
	}
}

func noSpace(pool string) error {
	return &models.Error{Code: models.CodeInsufficientSpace,
		Message: "the directory pool " + pool + " does not have room for this disk"}
}

// zeroFill writes zeros to [from, to) of f, checking ctx between chunks.
func zeroFill(ctx context.Context, f *os.File, from, to int64) error {
	buf := make([]byte, zeroChunk)
	for off := from; off < to; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(buf)), to-off)
		if _, err := f.WriteAt(buf[:n], off); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// syncDir makes a new or removed directory entry durable.
func syncRootDir(root *os.Root, rel string) error {
	dir, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// CreateFile implements Dirs.
func (d *dirs) CreateFile(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	p, root, st, rel, err := d.diskFile(spec)
	if err != nil {
		return Dataset{}, err
	}
	defer root.Close()
	if spec.SizeBytes <= 0 || spec.SizeBytes%512 != 0 {
		return Dataset{}, invalid("a disk file's size must be a positive multiple of 512 bytes")
	}
	if !spec.Thin && spec.SizeBytes > st.AvailableBytes {
		return Dataset{}, noSpace(spec.Pool)
	}
	f, err := root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return Dataset{}, fmt.Errorf("hostops: refusing to create %s in %s: it exists", rel, p.Path)
	}
	if err != nil {
		return Dataset{}, err
	}
	err = f.Truncate(spec.SizeBytes)
	if err == nil && !spec.Thin {
		err = zeroFill(ctx, f, 0, spec.SizeBytes)
	}
	if err == nil {
		err = f.Sync()
	}
	var fi fs.FileInfo
	if err == nil {
		fi, err = f.Stat()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// Never leave a disk file the database does not describe.
		if rerr := root.Remove(rel); rerr != nil {
			d.log.Warn("hostops: could not remove a disk file whose creation failed", "file", rel, "error", rerr)
		}
		if errors.Is(err, syscall.ENOSPC) {
			return Dataset{}, noSpace(spec.Pool)
		}
		return Dataset{}, err
	}
	if err := syncRootDir(root, path.Dir(rel)); err != nil {
		return Dataset{}, err
	}
	return fileDataset(p, rel, spec, fi), nil
}

// ResizeFile implements Dirs.
func (d *dirs) ResizeFile(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	p, root, st, rel, err := d.diskFile(spec)
	if err != nil {
		return Dataset{}, err
	}
	defer root.Close()
	fi, err := regularFile(root, rel)
	if err != nil {
		return Dataset{}, err
	}
	old := fi.Size()
	switch {
	case spec.SizeBytes <= old:
		return Dataset{}, invalid("a disk only grows: the new size must be larger than %d bytes", old)
	case spec.SizeBytes%512 != 0:
		return Dataset{}, invalid("the new size must be a multiple of 512 bytes")
	case !spec.Thin && spec.SizeBytes-old > st.AvailableBytes:
		return Dataset{}, noSpace(spec.Pool)
	}
	f, err := root.OpenFile(rel, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Dataset{}, err
	}
	err = f.Truncate(spec.SizeBytes)
	if err == nil && !spec.Thin {
		err = zeroFill(ctx, f, old, spec.SizeBytes)
	}
	if err != nil {
		// The guest keeps the size it had rather than a half-grown disk.
		if terr := f.Truncate(old); terr != nil {
			d.log.Warn("hostops: could not shrink a disk file back after a failed growth", "file", rel, "error", terr)
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		fi, err = f.Stat()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.ENOSPC) {
		return Dataset{}, noSpace(spec.Pool)
	}
	if err != nil {
		return Dataset{}, err
	}
	return fileDataset(p, rel, spec, fi), nil
}

// DestroyFile implements Dirs.
func (d *dirs) DestroyFile(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	p, root, _, rel, err := d.diskFile(spec)
	if err != nil {
		return Dataset{}, err
	}
	defer root.Close()
	fi, err := regularFile(root, rel)
	if err != nil {
		return Dataset{}, err
	}
	gone := fileDataset(p, rel, spec, fi)
	if err := root.Remove(rel); err != nil {
		return Dataset{}, err
	}
	if err := syncRootDir(root, path.Dir(rel)); err != nil {
		return Dataset{}, err
	}
	return gone, nil
}

// vmFiles lists a VM's directory. ok is false when it does not exist.
func (d *dirs) vmFiles(root *os.Root, p DirPool, rel, vmID string) ([]Dataset, bool, error) {
	fi, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !fi.IsDir() {
		return nil, false, fmt.Errorf("hostops: refusing %s in %s: it is not a directory", rel, p.Path)
	}
	dir, err := root.Open(rel)
	if err != nil {
		return nil, false, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, false, err
	}
	// Slot order, as zfs(8) lists a VM's zvols by name.
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return cmp.Or(cmp.Compare(len(a.Name()), len(b.Name())), cmp.Compare(a.Name(), b.Name()))
	})
	out := make([]Dataset, 0, len(entries))
	for _, e := range entries {
		if !diskFileRE.MatchString(e.Name()) || !e.Type().IsRegular() {
			return nil, false, fmt.Errorf("hostops: refusing %s in %s: it holds %q, which Skep did not create", rel, p.Path, e.Name())
		}
		info, err := e.Info()
		if err != nil {
			return nil, false, err
		}
		out = append(out, fileDataset(p, rel+"/"+e.Name(), VolumeSpec{VMID: vmID}, info))
	}
	return out, true, nil
}

// VMFiles implements Dirs.
func (d *dirs) VMFiles(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	p, rel, err := d.conf.DirVMRel(pool, vmID)
	if err != nil {
		return nil, err
	}
	root, _, err := d.open(p)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	list, _, err := d.vmFiles(root, p, rel, vmID)
	return list, err
}

// DestroyVM implements Dirs.
func (d *dirs) DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	p, rel, err := d.conf.DirVMRel(pool, vmID)
	if err != nil {
		return nil, err
	}
	root, _, err := d.open(p)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	list, ok, err := d.vmFiles(root, p, rel, vmID)
	if err != nil || !ok {
		return nil, err
	}
	var gone []Dataset
	for _, f := range list {
		if err := root.Remove(rel + "/" + filepath.Base(f.Name)); err != nil {
			return gone, err
		}
		gone = append(gone, f)
	}
	if err := root.Remove(rel); err != nil {
		return gone, err
	}
	gone = append(gone, Dataset{Name: filepath.Join(p.Path, rel), Type: "directory", VMID: vmID})
	return gone, syncRootDir(root, path.Dir(rel))
}
