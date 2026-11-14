package hostops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Media implements Files. Only regular files with an ISO name are listed;
// anything else in the directory is ignored and logged once. A missing
// directory is an empty listing.
func (h *host) Media(ctx context.Context) ([]MediaFile, error) {
	entries, err := os.ReadDir(h.conf.MediaDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []MediaFile{}, nil
		}
		return nil, fmt.Errorf("hostops: read the media directory: %w", err)
	}
	list := []MediaFile{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		// Info does not follow a symbolic link, so a link is never a
		// regular file here and cannot point the listing elsewhere.
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !ValidMediaName(name) {
			h.ignoreMedia(name)
			continue
		}
		list = append(list, MediaFile{Name: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (h *host) ignoreMedia(name string) {
	h.mediaMu.Lock()
	defer h.mediaMu.Unlock()
	if h.ignoredMedia[name] || len(h.ignoredMedia) >= 256 {
		return
	}
	h.ignoredMedia[name] = true
	h.log.Warn("hostops: ignoring an entry of the media directory: only regular files named *.iso are listed",
		"entry", printable(name))
}

// ResolveMedia implements Files. The name is looked up in the listing; it
// is never joined into a path before that.
func (h *host) ResolveMedia(ctx context.Context, name string) (string, error) {
	notFound := &models.Error{
		Code:    models.CodeMediaNotFound,
		Message: "the install media is not in the media directory",
		Details: map[string]any{"media": printable(name)},
	}
	if !ValidMediaName(name) {
		return "", notFound
	}
	list, err := h.Media(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range list {
		if m.Name == name {
			return filepath.Join(h.conf.MediaDir, name), nil
		}
	}
	return "", notFound
}

// PrepareVM implements Files.
func (h *host) PrepareVM(ctx context.Context, vmID string) error {
	dir, err := h.conf.VMStateDir(vmID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.conf.StateDir, 0o700); err != nil {
		return fmt.Errorf("hostops: create the VM state directory: %w", err)
	}
	if err := mkdirDurable(dir); err != nil {
		return fmt.Errorf("hostops: create the VM state directory: %w", err)
	}
	vars := filepath.Join(dir, VMVarsFile)
	if _, err := os.Lstat(vars); err == nil {
		return nil // the VM keeps the variables it has
	}
	return copyFile(ctx, filepath.Join(h.conf.FirmwareDir, FirmwareVarsFile), vars)
}

// maxVarsBytes bounds the UEFI variable store template: the real one is a
// few hundred kilobytes.
const maxVarsBytes = 64 << 20

// copyFile copies the firmware's variable store template; a missing
// template means bhyve cannot run a VM here.
func copyFile(ctx context.Context, from, to string) error {
	err := copyDurable(ctx, from, to)
	if errors.Is(err, fs.ErrNotExist) {
		return &models.Error{
			Code:    models.CodeBhyveUnavailable,
			Message: "the UEFI variable store template " + FirmwareVarsFile + " is not in the firmware directory",
		}
	}
	return err
}

// copyDurable copies a small file to a new root-only file, written under a
// temporary name, fsynced, renamed, and the directory fsynced, so a partial
// or lost copy is never used after a crash. A missing source is
// fs.ErrNotExist; neither end may be a symbolic link.
func copyDurable(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := os.Lstat(from); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("hostops: %s is not a regular file", from)
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := to + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("hostops: create %s: %w", tmp, err)
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxVarsBytes+1))
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxVarsBytes {
		err = errors.New("the variable store is implausibly large")
	}
	if err == nil {
		err = os.Rename(tmp, to)
	}
	if err == nil {
		err = syncDir(filepath.Dir(to))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hostops: copy the variable store: %w", err)
	}
	return nil
}

// VarsSnapshotDir is the directory, in a VM's persistent directory, of the
// copies of its UEFI variable store taken with its snapshots (ADR 0010
// decision 2b).
const VarsSnapshotDir = "snapshots"

// varsSnapshotPath is <state>/<vm-id>/snapshots/<snapshot-id>.fd.
func (h *host) varsSnapshotPath(vmID, snapshotID string) (string, error) {
	dir, err := h.conf.VMStateDir(vmID)
	if err != nil {
		return "", err
	}
	if !ValidID(snapshotID) {
		return "", invalid("invalid snapshot ID")
	}
	return filepath.Join(dir, VarsSnapshotDir, snapshotID+".fd"), nil
}

// SaveVarsSnapshot implements Files.
func (h *host) SaveVarsSnapshot(ctx context.Context, vmID, snapshotID string) error {
	to, err := h.varsSnapshotPath(vmID, snapshotID)
	if err != nil {
		return err
	}
	from, _ := h.conf.VMVarsPath(vmID)
	if err := mkdirDurable(filepath.Dir(to)); err != nil {
		return fmt.Errorf("hostops: create the VM's snapshot directory: %w", err)
	}
	if err := copyDurable(ctx, from, to); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("hostops: the VM %s has no UEFI variable store", vmID)
		}
		return err
	}
	return nil
}

// RestoreVarsSnapshot implements Files.
func (h *host) RestoreVarsSnapshot(ctx context.Context, vmID, snapshotID string) error {
	from, err := h.varsSnapshotPath(vmID, snapshotID)
	if err != nil {
		return err
	}
	to, _ := h.conf.VMVarsPath(vmID)
	if err := copyDurable(ctx, from, to); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("hostops: the snapshot %s has no copy of the UEFI variable store", snapshotID)
		}
		return err
	}
	return nil
}

// RemoveVarsSnapshot implements Files.
func (h *host) RemoveVarsSnapshot(vmID, snapshotID string) error {
	path, err := h.varsSnapshotPath(vmID, snapshotID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hostops: remove a snapshot's variable store: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// CloneVars implements Files.
func (h *host) CloneVars(ctx context.Context, sourceVMID, snapshotID, targetVMID string) error {
	from, err := h.varsSnapshotPath(sourceVMID, snapshotID)
	if err != nil {
		return err
	}
	dir, err := h.conf.VMStateDir(targetVMID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.conf.StateDir, 0o700); err != nil {
		return fmt.Errorf("hostops: create the VM state directory: %w", err)
	}
	if err := mkdirDurable(dir); err != nil {
		return fmt.Errorf("hostops: create the VM state directory: %w", err)
	}
	if err := copyDurable(ctx, from, filepath.Join(dir, VMVarsFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("hostops: the snapshot %s has no copy of the UEFI variable store", snapshotID)
		}
		return err
	}
	return nil
}

// RemoveVM implements Files.
func (h *host) RemoveVM(_ context.Context, vmID string) error {
	state, err := h.conf.VMStateDir(vmID)
	if err != nil {
		return err
	}
	run, err := h.conf.VMRunDir(vmID)
	if err != nil {
		return err
	}
	for _, dir := range []string{run, state} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("hostops: remove the VM's directory: %w", err)
		}
	}
	return nil
}
