package hostfake

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The fake's snapshots, clones, and inventory follow the real rules in
// internal/hostops/snapshot.go; the tests of the vm and storage packages
// run against them. Encryption is simulated by Lock.

// under reports whether a dataset or snapshot name is name or below it.
func under(full, name string) bool {
	base, _, _ := strings.Cut(full, "@")
	return base == name || strings.HasPrefix(base, name+"/")
}

// Lock makes a dataset encrypted with its key unloaded, or loaded again.
func (f *Fake) Lock(name string, locked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[name]
	if !ok {
		d = hostops.Dataset{Name: name, Type: "filesystem"}
	}
	d.Encryption, d.KeyStatus = "aes-256-gcm", "available"
	if locked {
		d.KeyStatus = "unavailable"
	}
	f.datasets[name] = d
}

// AddDataset adds a dataset as an operator would by hand, for the
// inventory's unowned case.
func (f *Fake) AddDataset(d hostops.Dataset) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txg++
	d.CreateTxg = f.txg
	f.datasets[d.Name] = d
}

// lockedAbove refuses a create under a dataset whose key is unloaded.
func (f *Fake) lockedAbove(name string) error {
	for dsName, d := range f.datasets {
		if d.Locked() && strings.HasPrefix(name, dsName+"/") {
			return &models.Error{Code: models.CodeStorageLocked,
				Message: "the encryption key of " + dsName + " is not loaded; an administrator loads it, Skep does not",
				Details: map[string]any{"dataset": dsName}}
		}
	}
	return nil
}

func (f *Fake) clonesUnder(name string) []string {
	var out []string
	for snapName, s := range f.snaps {
		if under(snapName, name) {
			out = append(out, s.Clones...)
		}
	}
	sort.Strings(out)
	return out
}

// dropOrigin removes a destroyed clone from its origin's clone list.
func (f *Fake) dropOrigin(d hostops.Dataset) {
	if d.Origin == "" {
		return
	}
	s, ok := f.snaps[d.Origin]
	if !ok {
		return
	}
	kept := s.Clones[:0:0]
	for _, c := range s.Clones {
		if c != d.Name {
			kept = append(kept, c)
		}
	}
	s.Clones = kept
	f.snaps[d.Origin] = s
}

func ownerProp(kind string, d hostops.Dataset) string {
	if kind == hostops.OwnerContainer {
		return d.ContainerID
	}
	return d.VMID
}

// tree returns the datasets of a resource, checked for ownership, and its
// snapshots, oldest first.
func (f *Fake) tree(pool string, o hostops.Owner) (string, []hostops.Dataset, []hostops.Dataset, error) {
	name, err := f.conf.OwnerDataset(pool, o)
	if err != nil {
		return "", nil, nil, err
	}
	if err := f.zfsLoaded(); err != nil {
		return "", nil, nil, err
	}
	list := f.vmDatasets(name)
	for _, d := range list {
		if ownerProp(o.Kind, d) != o.ID {
			return "", nil, nil, fmt.Errorf("hostfake: refusing %s: its owner property does not name %s %s", d.Name, o.Kind, o.ID)
		}
	}
	var snaps []hostops.Dataset
	for snapName, s := range f.snaps {
		if under(snapName, name) {
			snaps = append(snaps, s)
		}
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].CreateTxg != snaps[j].CreateTxg {
			return snaps[i].CreateTxg < snaps[j].CreateTxg
		}
		return snaps[i].Name < snaps[j].Name
	})
	return name, list, snaps, nil
}

func checkUnlocked(list []hostops.Dataset) error {
	for _, d := range list {
		if d.Locked() {
			return &models.Error{Code: models.CodeStorageLocked,
				Message: "the encryption key of " + d.Name + " is not loaded; an administrator loads it, Skep does not",
				Details: map[string]any{"dataset": d.Name}}
		}
	}
	return nil
}

// complete returns the snapshot of every dataset of the tree, as the real
// implementation does.
func complete(tree, snaps []hostops.Dataset, snapshotID string) ([]hostops.Dataset, error) {
	short, err := hostops.SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	byName := map[string]hostops.Dataset{}
	for _, s := range snaps {
		byName[s.Name] = s
	}
	var have []hostops.Dataset
	var missing []string
	for _, d := range tree {
		s, ok := byName[d.Name+"@"+short]
		if !ok {
			missing = append(missing, d.Name)
			continue
		}
		have = append(have, s)
	}
	switch {
	case len(have) == 0:
		return nil, fmt.Errorf("snapshot %s: %w", snapshotID, models.ErrNotFound)
	case len(missing) > 0:
		return nil, &models.Error{Code: models.CodeSnapshotIncomplete,
			Message: "the snapshot does not contain every disk the resource has now, so using it would make a state " +
				"the resource was never in; remove the disks added since the snapshot first",
			Details: map[string]any{"snapshot_id": snapshotID, "datasets_without_snapshot": missing}}
	}
	return have, nil
}

func hasClones(clones []string) error {
	sort.Strings(clones)
	return &models.Error{Code: models.CodeSnapshotHasClones,
		Message: "a linked clone depends on the snapshot; delete the clone first",
		Details: map[string]any{"clones": clones}}
}

// Inventory implements hostops.ZFS.
func (f *Fake) Inventory(_ context.Context, pool string) ([]hostops.Dataset, error) {
	root, err := f.conf.RootDataset(pool)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return nil, err
	}
	var list []hostops.Dataset
	for name, d := range f.datasets {
		if under(name, root) {
			list = append(list, d)
		}
	}
	for name, s := range f.snaps {
		if under(name, root) {
			list = append(list, s)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// Snapshot implements hostops.ZFS.
func (f *Fake) Snapshot(_ context.Context, pool string, o hostops.Owner, snapshotID string) ([]hostops.Dataset, error) {
	short, err := hostops.SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, tree, _, err := f.tree(pool, o)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, fmt.Errorf("the datasets of %s %s: %w", o.Kind, o.ID, models.ErrNotFound)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	for _, d := range tree {
		if _, ok := f.snaps[d.Name+"@"+short]; ok {
			return nil, fmt.Errorf("hostfake: snapshot %s@%s already exists", d.Name, short)
		}
	}
	f.txg++
	var made []hostops.Dataset
	for _, d := range tree {
		s := hostops.Dataset{Name: d.Name + "@" + short, Type: "snapshot", CreateTxg: f.txg,
			ReferencedBytes: d.ReferencedBytes, VolSizeBytes: d.VolSizeBytes, VMID: d.VMID, DiskID: d.DiskID, ContainerID: d.ContainerID,
			SnapshotID: snapshotID}
		f.snaps[s.Name] = s
		made = append(made, s)
	}
	f.record("zfs snapshot -r %s@%s", name, short)
	return made, nil
}

// Snapshots implements hostops.ZFS.
func (f *Fake) Snapshots(_ context.Context, pool string, o hostops.Owner) ([]hostops.Dataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _, snaps, err := f.tree(pool, o)
	return snaps, err
}

// Rollback implements hostops.ZFS.
func (f *Fake) Rollback(_ context.Context, pool string, o hostops.Owner, snapshotID string, destroyNewer bool) ([]hostops.Dataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, tree, snaps, err := f.tree(pool, o)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, fmt.Errorf("the datasets of %s %s: %w", o.Kind, o.ID, models.ErrNotFound)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	targets, err := complete(tree, snaps, snapshotID)
	if err != nil {
		return nil, err
	}
	var newer []hostops.Dataset
	for _, s := range snaps {
		if s.CreateTxg > targets[0].CreateTxg {
			newer = append(newer, s)
		}
	}
	if len(newer) > 0 {
		var ids, foreign, clones []string
		seen := map[string]bool{}
		for _, s := range newer {
			_, short, _ := strings.Cut(s.Name, "@")
			if s.SnapshotID == "" || short != hostops.SnapshotPrefix+s.SnapshotID {
				foreign = append(foreign, s.Name)
			} else if !seen[s.SnapshotID] {
				seen[s.SnapshotID] = true
				ids = append(ids, s.SnapshotID)
			}
			clones = append(clones, s.Clones...)
		}
		switch {
		case len(foreign) > 0:
			return nil, &models.Error{Code: models.CodeSnapshotNotLatest,
				Message: "newer snapshots that Skep did not take exist; an administrator removes them first",
				Details: map[string]any{"snapshot_id": snapshotID, "foreign_snapshots": foreign}}
		case !destroyNewer:
			return nil, &models.Error{Code: models.CodeSnapshotNotLatest,
				Message: "the snapshot is not the latest; rolling back to it destroys the newer ones, which needs destroy_newer",
				Details: map[string]any{"snapshot_id": snapshotID, "newer_snapshot_ids": ids}}
		case len(clones) > 0:
			return nil, hasClones(clones)
		}
		for _, s := range newer {
			delete(f.snaps, s.Name)
		}
	}
	for _, t := range targets {
		base, _, _ := strings.Cut(t.Name, "@")
		d := f.datasets[base]
		d.ReferencedBytes, d.VolSizeBytes = t.ReferencedBytes, t.VolSizeBytes
		if d.RefReservationBytes > 0 {
			d.RefReservationBytes = t.VolSizeBytes
		}
		f.datasets[base] = d
	}
	f.record("zfs rollback destroy_newer=%v %s@%s%s", destroyNewer, name, hostops.SnapshotPrefix, snapshotID)
	return newer, nil
}

// DestroySnapshot implements hostops.ZFS.
func (f *Fake) DestroySnapshot(_ context.Context, pool string, o hostops.Owner, snapshotID string) ([]hostops.Dataset, error) {
	short, err := hostops.SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, _, snaps, err := f.tree(pool, o)
	if err != nil {
		return nil, err
	}
	var mine []hostops.Dataset
	var clones []string
	for _, s := range snaps {
		if strings.HasSuffix(s.Name, "@"+short) {
			mine = append(mine, s)
			clones = append(clones, s.Clones...)
		}
	}
	if len(mine) == 0 {
		return nil, fmt.Errorf("snapshot %s: %w", snapshotID, models.ErrNotFound)
	}
	if len(clones) > 0 {
		return nil, hasClones(clones)
	}
	for _, s := range mine {
		delete(f.snaps, s.Name)
	}
	f.record("zfs destroy -r %s@%s", name, short)
	return mine, nil
}

// CloneVM implements hostops.ZFS.
func (f *Fake) CloneVM(_ context.Context, spec hostops.CloneSpec) ([]hostops.Dataset, error) {
	short, err := hostops.SnapshotName(spec.SnapshotID)
	if err != nil {
		return nil, err
	}
	target, err := f.conf.VMDataset(spec.Pool, spec.TargetVMID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, tree, snaps, err := f.tree(spec.Pool, hostops.Owner{Kind: hostops.OwnerVM, ID: spec.SourceVMID})
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, fmt.Errorf("the datasets of VM %s: %w", spec.SourceVMID, models.ErrNotFound)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	if _, err := complete(tree, snaps, spec.SnapshotID); err != nil {
		return nil, err
	}
	if _, ok := f.datasets[target]; ok {
		return nil, fmt.Errorf("hostfake: the clone's dataset %s already exists", target)
	}
	slots := 0
	for _, d := range tree {
		if d.Name == name {
			continue
		}
		slot, err := strconv.Atoi(strings.TrimPrefix(d.Name, name+"/disk"))
		if err != nil || !hostops.ValidID(spec.Disks[slot]) {
			return nil, models.Invalid(fmt.Sprintf("the clone needs a valid disk ID for %s", d.Name))
		}
		slots++
	}
	if slots != len(spec.Disks) {
		return nil, models.Invalid("the clone names disk slots the snapshot does not have")
	}
	var made []hostops.Dataset
	for _, d := range tree {
		origin := d.Name + "@" + short
		c := hostops.Dataset{Name: target + strings.TrimPrefix(d.Name, name), Type: d.Type,
			VolSizeBytes: f.snaps[origin].VolSizeBytes, VolMode: d.VolMode, VolBlockSize: d.VolBlockSize,
			ReferencedBytes: f.snaps[origin].ReferencedBytes, Origin: origin, VMID: spec.TargetVMID}
		if d.Type == "volume" {
			slot, _ := strconv.Atoi(strings.TrimPrefix(d.Name, name+"/disk"))
			c.DiskID = spec.Disks[slot]
		}
		f.txg++
		c.CreateTxg = f.txg
		f.datasets[c.Name] = c
		s := f.snaps[origin]
		s.Clones = append(s.Clones, c.Name)
		f.snaps[origin] = s
		made = append(made, c)
	}
	sort.Slice(made, func(i, j int) bool { return made[i].Name < made[j].Name })
	f.record("zfs clone %s@%s %s", name, short, target)
	return made, nil
}

// ResizeVolume implements hostops.ZFS.
func (f *Fake) ResizeVolume(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	name, err := f.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return hostops.Dataset{}, err
	}
	d, ok := f.datasets[name]
	if !ok || d.Type != "volume" || d.VMID != spec.VMID || d.DiskID != spec.DiskID {
		return hostops.Dataset{}, fmt.Errorf("hostfake: refusing %s: not the volume of disk %s", name, spec.DiskID)
	}
	if spec.SizeBytes <= d.VolSizeBytes || spec.SizeBytes%int64(d.VolBlockSize) != 0 {
		return hostops.Dataset{}, models.Invalid("a disk only grows, by a multiple of its block size")
	}
	if d.RefReservationBytes > 0 {
		grow := spec.SizeBytes - d.VolSizeBytes
		if grow > f.pool(spec.Pool).AvailableBytes {
			return hostops.Dataset{}, &models.Error{Code: models.CodeInsufficientSpace,
				Message: "the pool " + spec.Pool + " does not have room to grow this disk"}
		}
		d.RefReservationBytes, d.UsedBytes = spec.SizeBytes, spec.SizeBytes
	}
	d.VolSizeBytes = spec.SizeBytes
	f.datasets[name] = d
	f.record("zfs set volsize=%d %s", spec.SizeBytes, name)
	return d, nil
}

// DestroyVolume implements hostops.ZFS.
func (f *Fake) DestroyVolume(_ context.Context, spec hostops.VolumeSpec) (hostops.Dataset, error) {
	name, err := f.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return hostops.Dataset{}, err
	}
	d, ok := f.datasets[name]
	if !ok || d.Type != "volume" || d.VMID != spec.VMID || d.DiskID != spec.DiskID {
		return hostops.Dataset{}, fmt.Errorf("hostfake: refusing %s: not the volume of disk %s", name, spec.DiskID)
	}
	for snapName := range f.snaps {
		if strings.HasPrefix(snapName, name+"@") {
			return hostops.Dataset{}, fmt.Errorf("hostfake: refusing to destroy %s: it still has snapshots", name)
		}
	}
	delete(f.datasets, name)
	f.record("zfs destroy %s", name)
	return d, nil
}
