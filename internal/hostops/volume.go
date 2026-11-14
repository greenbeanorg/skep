package hostops

import (
	"context"
	"fmt"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// ownedVolume reads the zvol of a VM's disk and checks that it is one:
// a volume carrying the VM's and the disk's IDs.
func (h *host) ownedVolume(ctx context.Context, spec VolumeSpec) (string, Dataset, error) {
	name, err := h.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return "", Dataset{}, err
	}
	if !ValidID(spec.DiskID) {
		return "", Dataset{}, invalid("invalid disk ID")
	}
	if err := h.zfsLoaded(); err != nil {
		return "", Dataset{}, err
	}
	list, err := h.listDatasets(ctx, name, false)
	if err != nil {
		return "", Dataset{}, err
	}
	if len(list) != 1 {
		return "", Dataset{}, notFound("the volume " + name)
	}
	d := list[0]
	if d.Type != "volume" || d.VMID != spec.VMID || d.DiskID != spec.DiskID {
		return "", Dataset{}, fmt.Errorf("hostops: refusing %s: it is not the volume of disk %s of VM %s", name, spec.DiskID, spec.VMID)
	}
	if err := checkUnlocked(list); err != nil {
		return "", Dataset{}, err
	}
	return name, d, nil
}

// ResizeVolume implements ZFS.
func (h *host) ResizeVolume(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	name, d, err := h.ownedVolume(ctx, spec)
	if err != nil {
		return Dataset{}, err
	}
	bs := int64(d.VolBlockSize)
	switch {
	case spec.SizeBytes <= d.VolSizeBytes:
		return Dataset{}, invalid("a disk only grows: the new size must be larger than %d bytes", d.VolSizeBytes)
	case bs <= 0 || spec.SizeBytes%bs != 0:
		return Dataset{}, invalid("the new size must be a multiple of the volume block size, %d bytes", bs)
	}
	// zfs(8) keeps the reservation of a volume that has one equal to its
	// size when volsize changes; a thin volume stays without one.
	if _, err := h.run.Run(ctx, zfsBinary, "set", "volsize="+strconv.FormatInt(spec.SizeBytes, 10), name); err != nil {
		if stderrContains(err, "out of space") || stderrContains(err, "size is greater than available space") {
			return Dataset{}, &models.Error{Code: models.CodeInsufficientSpace,
				Message: "the pool " + spec.Pool + " does not have room to grow this disk"}
		}
		return Dataset{}, err
	}
	return h.oneDataset(ctx, name)
}

// DestroyVolume implements ZFS. It never uses -r: a volume that still has
// snapshots is refused by zfs(8) as well as here.
func (h *host) DestroyVolume(ctx context.Context, spec VolumeSpec) (Dataset, error) {
	name, d, err := h.ownedVolume(ctx, spec)
	if err != nil {
		return Dataset{}, err
	}
	snaps, err := h.listSnapshots(ctx, name, false)
	if err != nil {
		return Dataset{}, err
	}
	if len(snaps) > 0 {
		return Dataset{}, fmt.Errorf("hostops: refusing to destroy %s: it still has %d snapshots", name, len(snaps))
	}
	if _, err := h.run.Run(ctx, zfsBinary, "destroy", name); err != nil {
		return Dataset{}, err
	}
	return d, nil
}
