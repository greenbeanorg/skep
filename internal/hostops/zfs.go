package hostops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// sysctlZFSLoaded exists only while the zfs kernel module is loaded.
const sysctlZFSLoaded = "vfs.zfs.version.module"

// User properties on every dataset Skep creates, so a stray dataset can
// be matched to its owner without guessing from its name.
const (
	propVMID        = "skep:vm_id"
	propDiskID      = "skep:disk_id"
	propContainerID = "skep:container_id"
	propTemplateID  = "skep:template_id"
	// propSnapshotID is set on snapshots, which take user properties.
	propSnapshotID = "skep:snapshot_id"
)

// Volume limits (ADR 0009 decision 3).
const (
	minVolBlockSize = 4 << 10
	maxVolBlockSize = 128 << 10
)

func storageUnavailable(message string) error {
	return &models.Error{Code: models.CodeStorageUnavailable, Message: message}
}

// zfsLoaded reports whether the zfs kernel module is loaded. zfs(8) and
// zpool(8) run as root load it on demand, and Skep loads nothing (ADR
// 0003), so nothing below runs a command before this says yes.
func (h *host) zfsLoaded() error {
	loaded, err := h.sys.SysctlExists(sysctlZFSLoaded)
	switch {
	case err != nil:
		return fmt.Errorf("hostops: cannot tell whether zfs is loaded: %w", err)
	case !loaded:
		return storageUnavailable("the zfs kernel module is not loaded on this host; an administrator enables ZFS, Skep does not")
	}
	return nil
}

// Pools implements ZFS.
func (h *host) Pools(ctx context.Context) ([]Pool, error) {
	out := make([]Pool, 0, len(h.conf.Pools))
	loaded := h.zfsLoaded()
	for _, name := range h.conf.Pools {
		p := Pool{Name: name}
		if loaded != nil {
			p.Error = "the zfs kernel module is not loaded"
			var me *models.Error
			if !errors.As(loaded, &me) {
				p.Error = "the state of the zfs kernel module could not be read"
			}
			out = append(out, p)
			continue
		}
		read, err := h.readPool(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			h.log.Warn("hostops: cannot read pool", "pool", name, "error", err)
			p.Error = "the pool is not imported or cannot be read"
			out = append(out, p)
			continue
		}
		out = append(out, read)
	}
	return out, nil
}

// Pool implements ZFS.
func (h *host) Pool(ctx context.Context, name string) (Pool, error) {
	if err := h.conf.CheckPool(name); err != nil {
		return Pool{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Pool{}, err
	}
	p, err := h.readPool(ctx, name)
	if err != nil {
		if ctx.Err() != nil {
			return Pool{}, ctx.Err()
		}
		h.log.Warn("hostops: cannot read pool", "pool", name, "error", err)
		return Pool{}, storageUnavailable("the pool " + name + " is not imported or cannot be read")
	}
	return p, nil
}

// readPool reads one pool. The caller checked the allow-list and that zfs
// is loaded.
func (h *host) readPool(ctx context.Context, name string) (Pool, error) {
	out, err := h.run.Run(ctx, zpoolBinary, "list", "-H", "-p", "-o", "name,size,allocated,free,health", name)
	if err != nil {
		return Pool{}, err
	}
	f := strings.Split(strings.TrimRight(string(out), "\r\n"), "\t")
	if len(f) != 5 || f[0] != name {
		return Pool{}, fmt.Errorf("zpool list: unexpected output %q", firstLine(out))
	}
	p := Pool{Name: name, Health: f[4]}
	for i, dst := range []*int64{&p.SizeBytes, &p.AllocatedBytes, &p.FreeBytes} {
		if *dst, err = parseBytes(f[i+1]); err != nil {
			return Pool{}, fmt.Errorf("zpool list: %w", err)
		}
	}
	out, err = h.run.Run(ctx, zfsBinary, "get", "-H", "-p", "-o", "value", "available", name)
	if err != nil {
		return Pool{}, err
	}
	if p.AvailableBytes, err = parseBytes(strings.TrimSpace(string(out))); err != nil {
		return Pool{}, fmt.Errorf("zfs get available: %w", err)
	}
	return p, nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}

// parseBytes parses a number printed by zfs or zpool with -p. "-" (not
// applicable) and "none" are zero.
func parseBytes(s string) (int64, error) {
	if s == "-" || s == "none" || s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid number %q", s)
	}
	return n, nil
}

// datasetColumns is the fixed column list of every dataset listing. With
// -p, sizes and createtxg are plain integers and compressratio is a bare
// decimal; a property that does not apply prints "-".
var datasetColumns = strings.Join([]string{
	"name", "type", "volsize", "used", "referenced", "refreservation", "volmode", "volblocksize",
	"createtxg", "origin", "clones", "encryption", "keystatus", "compression", "compressratio",
	propVMID, propDiskID, propContainerID, propTemplateID, propSnapshotID, "refquota",
}, ",")

const datasetFields = 21

// listDatasets lists a dataset, recursively when asked. A dataset that
// does not exist yields an empty list.
func (h *host) listDatasets(ctx context.Context, name string, recursive bool) ([]Dataset, error) {
	return h.list(ctx, name, "filesystem,volume", recursive)
}

// listSnapshots lists the snapshots of a dataset and, when asked, of
// everything under it, oldest first.
func (h *host) listSnapshots(ctx context.Context, name string, recursive bool) ([]Dataset, error) {
	list, err := h.list(ctx, name, "snapshot", recursive)
	sortByTxg(list)
	return list, err
}

func (h *host) list(ctx context.Context, name, types string, recursive bool) ([]Dataset, error) {
	args := []string{"list", "-H", "-p", "-t", types, "-o", datasetColumns}
	if recursive {
		args = append(args, "-r")
	} else if types == "snapshot" {
		// Without -r, zfs list of a filesystem lists the filesystem;
		// -d 1 lists its own snapshots.
		args = append(args, "-d", "1")
	}
	out, err := h.run.Run(ctx, zfsBinary, append(args, name)...)
	if err != nil {
		if stderrContains(err, "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	return parseDatasets(out)
}

// sortByTxg orders snapshots by the transaction group that created them,
// which is the order ZFS rolls back in; the name breaks ties, so the
// snapshots of one recursive zfs snapshot keep a stable order.
func sortByTxg(list []Dataset) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].CreateTxg != list[j].CreateTxg {
			return list[i].CreateTxg < list[j].CreateTxg
		}
		return list[i].Name < list[j].Name
	})
}

func parseDatasets(out []byte) ([]Dataset, error) {
	var list []Dataset
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != datasetFields {
			return nil, fmt.Errorf("zfs list: want %d tab-separated fields, got %d", datasetFields, len(f))
		}
		d := Dataset{
			Name: f[0], Type: f[1], VolMode: dash(f[6]), Origin: dash(f[9]),
			Encryption: dash(f[11]), KeyStatus: dash(f[12]), Compression: dash(f[13]), CompressRatio: dash(f[14]),
			VMID: dash(f[15]), DiskID: dash(f[16]), ContainerID: dash(f[17]), TemplateID: dash(f[18]), SnapshotID: dash(f[19]),
		}
		if c := dash(f[10]); c != "" {
			d.Clones = strings.Split(c, ",")
		}
		var err error
		for i, dst := range []*int64{&d.VolSizeBytes, &d.UsedBytes, &d.ReferencedBytes, &d.RefReservationBytes} {
			if *dst, err = parseBytes(f[i+2]); err != nil {
				return nil, fmt.Errorf("zfs list: %s: %w", f[0], err)
			}
		}
		bs, err := parseBytes(f[7])
		if err != nil {
			return nil, fmt.Errorf("zfs list: %s: %w", f[0], err)
		}
		d.VolBlockSize = int(bs)
		if d.RefQuotaBytes, err = parseBytes(f[20]); err != nil {
			return nil, fmt.Errorf("zfs list: %s: %w", f[0], err)
		}
		if d.CreateTxg, err = parseBytes(f[8]); err != nil {
			return nil, fmt.Errorf("zfs list: %s: %w", f[0], err)
		}
		list = append(list, d)
	}
	return list, sc.Err()
}

// dash turns the "-" zfs prints for a property that is not set into "".
func dash(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

// ensureUnlockedDataset creates a container dataset if it does not exist,
// and refuses one that exists with its key unloaded. It is never mounted:
// it only holds other datasets.
func (h *host) ensureUnlockedDataset(ctx context.Context, name string, props ...string) error {
	existing, err := h.listDatasets(ctx, name, false)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return checkUnlocked(existing)
	}
	args := []string{"create", "-o", "canmount=off"}
	for _, p := range props {
		args = append(args, "-o", p)
	}
	_, err = h.run.Run(ctx, zfsBinary, append(args, name)...)
	return err
}

// CreateVMDataset implements ZFS.
func (h *host) CreateVMDataset(ctx context.Context, pool, vmID string) (Dataset, error) {
	name, err := h.conf.VMDataset(pool, vmID)
	if err != nil {
		return Dataset{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Dataset{}, err
	}
	// <pool>/<root>, each component of the root, then <root>/vm. Each
	// that exists must have its key loaded (ADR 0010 decision 5): an
	// encrypted <pool>/<root> is the operator's, and so is its key.
	parent := pool
	for _, part := range append(strings.Split(h.conf.DatasetRoot, "/"), "vm") {
		parent += "/" + part
		if err := h.ensureUnlockedDataset(ctx, parent); err != nil {
			return Dataset{}, err
		}
	}
	if err := h.ensureUnlockedDataset(ctx, name, propVMID+"="+vmID); err != nil {
		return Dataset{}, err
	}
	return h.oneDataset(ctx, name)
}

func (h *host) oneDataset(ctx context.Context, name string) (Dataset, error) {
	list, err := h.listDatasets(ctx, name, false)
	if err != nil {
		return Dataset{}, err
	}
	if len(list) != 1 {
		return Dataset{}, fmt.Errorf("hostops: dataset %s does not exist after it was created", name)
	}
	return list[0], nil
}

// validVolume checks the typed values of a volume. The service validated
// them for the user; this is the last line before the command.
func validVolume(spec VolumeSpec) error {
	bs := int64(spec.BlockSize)
	switch {
	case !ValidID(spec.DiskID):
		return invalid("invalid disk ID")
	case bs < minVolBlockSize || bs > maxVolBlockSize || bs&(bs-1) != 0:
		return invalid("the volume block size must be a power of two from 4K to 128K")
	case spec.SizeBytes < models.MinDiskBytes || spec.SizeBytes%bs != 0:
		return invalid("the volume size must be at least 16 MiB and a multiple of its block size")
	}
	return nil
}

// CreateVolume implements ZFS.
func (h *host) CreateVolume(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	name, err := h.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return Dataset{}, err
	}
	if err := validVolume(spec); err != nil {
		return Dataset{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Dataset{}, err
	}
	parent, _ := h.conf.VMDataset(spec.Pool, spec.VMID)
	if above, err := h.listDatasets(ctx, parent, false); err != nil {
		return Dataset{}, err
	} else if err := checkUnlocked(above); err != nil {
		return Dataset{}, err
	}
	args := []string{"create"}
	if spec.Thin {
		args = append(args, "-s")
	}
	args = append(args,
		"-V", strconv.FormatInt(spec.SizeBytes, 10),
		"-o", "volblocksize="+strconv.Itoa(spec.BlockSize),
		// The zvol is a device node and nothing else: the host never
		// tastes the guest's partition table.
		"-o", "volmode=dev",
		"-o", propVMID+"="+spec.VMID,
		"-o", propDiskID+"="+spec.DiskID,
		name)
	if _, err := h.run.Run(ctx, zfsBinary, args...); err != nil {
		if stderrContains(err, "out of space") {
			return Dataset{}, &models.Error{
				Code:    models.CodeInsufficientSpace,
				Message: "the pool " + spec.Pool + " does not have room for this disk",
			}
		}
		return Dataset{}, err
	}
	return h.oneDataset(ctx, name)
}

// VMDatasets implements ZFS.
func (h *host) VMDatasets(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	name, err := h.conf.VMDataset(pool, vmID)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	return h.listDatasets(ctx, name, true)
}

// DestroyVM implements ZFS. It destroys with -r, which takes the dataset,
// its children, and their snapshots, and never with -R, which would also
// take clones that live elsewhere.
func (h *host) DestroyVM(ctx context.Context, pool, vmID string) ([]Dataset, error) {
	name, err := h.conf.VMDataset(pool, vmID)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	list, err := h.listDatasets(ctx, name, true)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	// The name is derived from the ID, and the dataset must say the
	// same about itself before it is destroyed.
	for _, d := range list {
		if d.Name != name && !strings.HasPrefix(d.Name, name+"/") {
			return nil, fmt.Errorf("hostops: refusing to destroy %s: it is outside %s", d.Name, name)
		}
		if d.VMID != vmID {
			return nil, fmt.Errorf("hostops: refusing to destroy %s: its %s property does not name this VM", d.Name, propVMID)
		}
	}
	// zfs destroy -r refuses a snapshot with clones too; this says which.
	snaps, err := h.listSnapshots(ctx, name, true)
	if err != nil {
		return nil, err
	}
	if clones := clonesOf(snaps); len(clones) > 0 {
		return nil, hasClones(clones)
	}
	if _, err := h.run.Run(ctx, zfsBinary, "destroy", "-r", name); err != nil {
		return nil, err
	}
	return list, nil
}
