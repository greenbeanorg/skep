package hostops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// ErrLeftoverDevfsNotRemoved marks a leftover devfs that was found and could
// not be unmounted, as opposed to a check that failed before finding one.
var ErrLeftoverDevfsNotRemoved = errors.New("a leftover devfs was not removed")

// fsMount is one entry of the host's mount table as getfsstat(2) reports
// it.
type fsMount struct {
	// On is f_mntonname, the path the file system was mounted on.
	On string
	// FSType is f_fstypename.
	FSType string
	// FSID is f_fsid.
	FSID [2]int32
}

// mountTable reads the host's mount table and unmounts one file system by
// its fsid (amendment l). Tests replace it.
type mountTable interface {
	Mounts() ([]fsMount, error)
	UnmountFSID(fsid [2]int32) error
}

// fsidString is the form unmount(2) takes with MNT_BYFSID, and the form
// the log records.
func fsidString(fsid [2]int32) string {
	return fmt.Sprintf("FSID:%d:%d", fsid[0], fsid[1])
}

// RemoveLeftoverDevfs implements Jails.
func (h *host) RemoveLeftoverDevfs(ctx context.Context, pool, containerID string) ([]string, error) {
	name, err := JailName(containerID)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	if running, err := h.jailExists(ctx, name); err != nil {
		return nil, err
	} else if running {
		return nil, containerNotStopped(containerID)
	}
	mp, err := h.jailsMount(ctx, pool)
	if err != nil {
		return nil, err
	}
	return h.removeLeftoverDevfs(containerID, filepath.Join(mp, containerID))
}

// removeLeftoverDevfs unmounts every devfs mounted exactly on <root>/dev
// (amendment l): a removal outside Skep (jail -r without Skep's
// jail.conf) can leave one, and it blocks the delete of the jail's
// dataset. The mount is found in the mount table, where f_mntonname must
// equal <root>/dev byte for byte and f_fstypename must be devfs, and it is
// unmounted by its fsid (MNT_BYFSID), never by a path: nothing is
// resolved, so a dev that is a symbolic link, or anything else the jail's
// root holds, cannot point the unmount at a host mount. The caller has
// checked that no jail of that name runs. The newest mount goes first.
// It returns the fsids it unmounted.
func (h *host) removeLeftoverDevfs(containerID, root string) ([]string, error) {
	if h.mounts == nil {
		return nil, fmt.Errorf("hostops: no mount table to look for a leftover devfs")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("hostops: refusing to look for a devfs under %q: not a clean absolute path", root)
	}
	want := root + "/dev"
	all, err := h.mounts.Mounts()
	if err != nil {
		return nil, fmt.Errorf("hostops: read the mount table: %w", err)
	}
	var found []fsMount
	for _, m := range all {
		if m.On == want && m.FSType == "devfs" {
			found = append(found, m)
		}
	}
	var removed []string
	for i := len(found) - 1; i >= 0; i-- {
		m := found[i]
		if m.FSID == ([2]int32{}) {
			return removed, fmt.Errorf("hostops: the devfs on %s has no fsid; not unmounting it: %w", want, ErrLeftoverDevfsNotRemoved)
		}
		if err := h.mounts.UnmountFSID(m.FSID); err != nil {
			return removed, fmt.Errorf("hostops: unmount the devfs %s on %s: %w: %w", fsidString(m.FSID), want, ErrLeftoverDevfsNotRemoved, err)
		}
		removed = append(removed, fsidString(m.FSID))
	}
	if len(removed) > 0 {
		h.log.Warn("hostops: unmounted a devfs a removal outside Skep left on the jail's root", "event", "orphaned_devfs_removed",
			"container_id", containerID, "mountpoint", want, "fsids", removed)
	}
	return removed, nil
}
