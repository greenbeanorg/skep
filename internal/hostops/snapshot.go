package hostops

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Snapshots, rollback, linked clones, and the inventory (ADR 0010
// decisions 1 to 3 and 5). The rules, on top of the package's:
//
//   - A snapshot is one recursive zfs snapshot of a resource's dataset,
//     named @skep-<snapshot-id> and carrying skep:snapshot_id. Every call
//     that changes or destroys a snapshot first checks that every dataset
//     it touches lies in the resource's tree and carries the resource's
//     owner property, and that every snapshot carries the snapshot's ID.
//   - Nothing here uses zfs destroy -R or zfs promote: a clone is never
//     destroyed through its origin, and a snapshot never moves between
//     owners.
//   - A snapshot is used whole or not at all. A rollback or a clone of a
//     snapshot that some dataset of the resource lacks (a disk added after
//     it) is refused before anything changes.

// Inventory lists every filesystem, volume, and snapshot under
// <pool>/<root>, ordered by name. A pool without the root yields an empty
// list.
func (h *host) Inventory(ctx context.Context, pool string) ([]Dataset, error) {
	root, err := h.conf.rootDataset(pool)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	list, err := h.list(ctx, root, "filesystem,volume,snapshot", true)
	sortByName(list)
	return list, err
}

func sortByName(list []Dataset) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}

// ownerProp returns the value of the owner property a dataset carries for
// an owner kind.
func ownerProp(kind string, d Dataset) string {
	switch kind {
	case OwnerVM:
		return d.VMID
	case OwnerContainer:
		return d.ContainerID
	}
	return ""
}

// ownedTree lists the dataset of a resource and everything under it, and
// checks that each lies in the tree and says it belongs to the resource.
// A resource without a dataset in the pool yields an empty list.
func (h *host) ownedTree(ctx context.Context, pool string, o Owner) (string, []Dataset, error) {
	name, err := h.conf.OwnerDataset(pool, o)
	if err != nil {
		return "", nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return "", nil, err
	}
	list, err := h.listDatasets(ctx, name, true)
	if err != nil {
		return "", nil, err
	}
	if err := checkOwned(name, o, list); err != nil {
		return "", nil, err
	}
	return name, list, nil
}

func checkOwned(name string, o Owner, list []Dataset) error {
	for _, d := range list {
		base, _, _ := strings.Cut(d.Name, "@")
		if base != name && !strings.HasPrefix(base, name+"/") {
			return fmt.Errorf("hostops: refusing %s: it is outside %s", d.Name, name)
		}
		if ownerProp(o.Kind, d) != o.ID {
			return fmt.Errorf("hostops: refusing %s: its owner property does not name %s %s", d.Name, o.Kind, o.ID)
		}
	}
	return nil
}

// checkUnlocked refuses datasets whose encryption key is not loaded (ADR
// 0010 decision 5). Skep holds no key and loads none.
func checkUnlocked(list []Dataset) error {
	for _, d := range list {
		if d.Locked() {
			return &models.Error{
				Code:    models.CodeStorageLocked,
				Message: "the encryption key of " + d.Name + " is not loaded; an administrator loads it, Skep does not",
				Details: map[string]any{"dataset": d.Name},
			}
		}
	}
	return nil
}

func notFound(what string) error { return fmt.Errorf("%s: %w", what, models.ErrNotFound) }

// snapshotIndex is the snapshots of one resource's tree, by dataset.
type snapshotIndex map[string][]Dataset

func indexSnapshots(list []Dataset) snapshotIndex {
	idx := snapshotIndex{}
	for _, s := range list {
		base, _, _ := strings.Cut(s.Name, "@")
		idx[base] = append(idx[base], s)
	}
	return idx
}

// find returns the snapshot @short of a dataset.
func (idx snapshotIndex) find(dataset, short string) (Dataset, bool) {
	for _, s := range idx[dataset] {
		if s.Name == dataset+"@"+short {
			return s, true
		}
	}
	return Dataset{}, false
}

// complete returns, for every dataset of the tree, its snapshot of the
// given ID. A snapshot that no dataset has is not found; one that only
// some have is snapshot_incomplete; a snapshot whose skep:snapshot_id does
// not match its name is refused.
func complete(tree []Dataset, idx snapshotIndex, snapshotID string) ([]Dataset, error) {
	short, err := SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	var have []Dataset
	var missing []string
	for _, d := range tree {
		s, ok := idx.find(d.Name, short)
		if !ok {
			missing = append(missing, d.Name)
			continue
		}
		if s.SnapshotID != snapshotID {
			return nil, fmt.Errorf("hostops: refusing %s: its %s property does not match its name", s.Name, propSnapshotID)
		}
		have = append(have, s)
	}
	switch {
	case len(have) == 0:
		return nil, notFound("snapshot " + snapshotID)
	case len(missing) > 0:
		return nil, &models.Error{
			Code: models.CodeSnapshotIncomplete,
			Message: "the snapshot does not contain every disk the resource has now, so using it would make a state " +
				"the resource was never in; remove the disks added since the snapshot first",
			Details: map[string]any{"snapshot_id": snapshotID, "datasets_without_snapshot": missing},
		}
	}
	return have, nil
}

// clonesOf collects the clones of a list of snapshots.
func clonesOf(list []Dataset) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Clones...)
	}
	sort.Strings(out)
	return out
}

func hasClones(clones []string) error {
	return &models.Error{
		Code:    models.CodeSnapshotHasClones,
		Message: "a linked clone depends on the snapshot; delete the clone first",
		Details: map[string]any{"clones": clones},
	}
}

// Snapshot implements ZFS.
func (h *host) Snapshot(ctx context.Context, pool string, o Owner, snapshotID string) ([]Dataset, error) {
	short, err := SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	name, tree, err := h.ownedTree(ctx, pool, o)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, notFound("the datasets of " + o.Kind + " " + o.ID)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	// One command, so ZFS takes every dataset's snapshot in the same
	// transaction group: atomic across the disks.
	if _, err := h.run.Run(ctx, zfsBinary, "snapshot", "-r", "-o", propSnapshotID+"="+snapshotID, name+"@"+short); err != nil {
		if stderrContains(err, "out of space") {
			return nil, &models.Error{Code: models.CodeInsufficientSpace, Message: "the pool " + pool + " does not have room for the snapshot"}
		}
		return nil, err
	}
	snaps, err := h.listSnapshots(ctx, name, true)
	if err != nil {
		return nil, err
	}
	return complete(tree, indexSnapshots(snaps), snapshotID)
}

// Snapshots implements ZFS.
func (h *host) Snapshots(ctx context.Context, pool string, o Owner) ([]Dataset, error) {
	name, err := h.conf.OwnerDataset(pool, o)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	return h.listSnapshots(ctx, name, true)
}

// Rollback implements ZFS.
func (h *host) Rollback(ctx context.Context, pool string, o Owner, snapshotID string, destroyNewer bool) ([]Dataset, error) {
	name, tree, err := h.ownedTree(ctx, pool, o)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, notFound("the datasets of " + o.Kind + " " + o.ID)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	snaps, err := h.listSnapshots(ctx, name, true)
	if err != nil {
		return nil, err
	}
	idx := indexSnapshots(snaps)
	targets, err := complete(tree, idx, snapshotID)
	if err != nil {
		return nil, err
	}
	// Everything newer than the target, on every dataset, in the order
	// it was taken.
	var newer []Dataset
	newerOn := map[string]bool{}
	for _, t := range targets {
		base, _, _ := strings.Cut(t.Name, "@")
		for _, s := range idx[base] {
			if s.CreateTxg > t.CreateTxg {
				newer = append(newer, s)
				newerOn[base] = true
			}
		}
	}
	sortByTxg(newer)
	if len(newer) > 0 {
		ids, foreign := newerIDs(newer)
		switch {
		case len(foreign) > 0:
			// A snapshot Skep did not take is not Skep's to destroy.
			return nil, &models.Error{
				Code:    models.CodeSnapshotNotLatest,
				Message: "newer snapshots that Skep did not take exist; an administrator removes them first",
				Details: map[string]any{"snapshot_id": snapshotID, "foreign_snapshots": foreign},
			}
		case !destroyNewer:
			return nil, &models.Error{
				Code:    models.CodeSnapshotNotLatest,
				Message: "the snapshot is not the latest; rolling back to it destroys the newer ones, which needs destroy_newer",
				Details: map[string]any{"snapshot_id": snapshotID, "newer_snapshot_ids": ids},
			}
		}
		if clones := clonesOf(newer); len(clones) > 0 {
			return nil, hasClones(clones)
		}
	}
	for _, t := range targets {
		base, _, _ := strings.Cut(t.Name, "@")
		args := []string{"rollback"}
		if newerOn[base] {
			// -r destroys the newer snapshots of this dataset, which
			// were all checked above. Never -R, which takes clones.
			args = append(args, "-r")
		}
		if _, err := h.run.Run(ctx, zfsBinary, append(args, t.Name)...); err != nil {
			return nil, err
		}
	}
	return newer, nil
}

// newerIDs returns the distinct snapshot IDs of Skep's snapshots, in
// order, and the names of the snapshots Skep did not take.
func newerIDs(list []Dataset) (ids, foreign []string) {
	seen := map[string]bool{}
	for _, s := range list {
		_, short, _ := strings.Cut(s.Name, "@")
		if s.SnapshotID == "" || short != SnapshotPrefix+s.SnapshotID {
			foreign = append(foreign, s.Name)
			continue
		}
		if !seen[s.SnapshotID] {
			seen[s.SnapshotID] = true
			ids = append(ids, s.SnapshotID)
		}
	}
	return ids, foreign
}

// DestroySnapshot implements ZFS.
func (h *host) DestroySnapshot(ctx context.Context, pool string, o Owner, snapshotID string) ([]Dataset, error) {
	short, err := SnapshotName(snapshotID)
	if err != nil {
		return nil, err
	}
	name, tree, err := h.ownedTree(ctx, pool, o)
	if err != nil {
		return nil, err
	}
	snaps, err := h.listSnapshots(ctx, name, true)
	if err != nil {
		return nil, err
	}
	inTree := map[string]bool{}
	for _, d := range tree {
		inTree[d.Name] = true
	}
	var mine []Dataset
	for _, s := range snaps {
		base, sn, _ := strings.Cut(s.Name, "@")
		if sn != short {
			continue
		}
		if !inTree[base] || s.SnapshotID != snapshotID {
			return nil, fmt.Errorf("hostops: refusing to destroy %s: it is not a snapshot Skep took of %s %s", s.Name, o.Kind, o.ID)
		}
		mine = append(mine, s)
	}
	if len(mine) == 0 {
		return nil, notFound("snapshot " + snapshotID)
	}
	if clones := clonesOf(mine); len(clones) > 0 {
		return nil, hasClones(clones)
	}
	if _, ok := indexSnapshots(mine).find(name, short); ok {
		// -r takes the snapshot of this name on every dataset below,
		// each of which was checked; ZFS destroys them together.
		_, err = h.run.Run(ctx, zfsBinary, "destroy", "-r", name+"@"+short)
		return mine, err
	}
	// The top dataset's snapshot is gone (removed by hand): destroy what
	// is left one by one.
	for _, s := range mine {
		if _, err := h.run.Run(ctx, zfsBinary, "destroy", s.Name); err != nil {
			return nil, err
		}
	}
	return mine, nil
}

// CloneSpec describes a linked clone of a VM from one of its snapshots.
type CloneSpec struct {
	Pool       string
	SourceVMID string
	SnapshotID string
	TargetVMID string
	// Disks maps every disk slot of the source's snapshot to the disk ID
	// of the clone's disk in that slot.
	Disks map[int]string
}

var diskSuffixRE = regexp.MustCompile(`^/disk([0-9]{1,2})$`)

// CloneVM implements ZFS. It returns the clone's datasets. When it fails
// part way, what it made carries the clone's owner property, so DestroyVM
// of the clone removes it without touching the source.
func (h *host) CloneVM(ctx context.Context, spec CloneSpec) ([]Dataset, error) {
	short, err := SnapshotName(spec.SnapshotID)
	if err != nil {
		return nil, err
	}
	target, err := h.conf.VMDataset(spec.Pool, spec.TargetVMID)
	if err != nil {
		return nil, err
	}
	if spec.TargetVMID == spec.SourceVMID {
		return nil, invalid("a clone needs a new VM ID")
	}
	name, tree, err := h.ownedTree(ctx, spec.Pool, Owner{Kind: OwnerVM, ID: spec.SourceVMID})
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, notFound("the datasets of VM " + spec.SourceVMID)
	}
	if err := checkUnlocked(tree); err != nil {
		return nil, err
	}
	snaps, err := h.listSnapshots(ctx, name, true)
	if err != nil {
		return nil, err
	}
	if _, err := complete(tree, indexSnapshots(snaps), spec.SnapshotID); err != nil {
		return nil, err
	}
	// The tree is the VM's filesystem and one zvol per disk slot, and
	// the caller named a disk ID for exactly those slots.
	slots := map[int]bool{}
	for _, d := range tree {
		if d.Name == name {
			continue
		}
		m := diskSuffixRE.FindStringSubmatch(strings.TrimPrefix(d.Name, name))
		if m == nil || d.Type != "volume" {
			return nil, fmt.Errorf("hostops: refusing to clone %s: %s is not a disk of the VM", name, d.Name)
		}
		slot, _ := strconv.Atoi(m[1])
		if _, ok := spec.Disks[slot]; !ok || !ValidID(spec.Disks[slot]) || slot > MaxSlot {
			return nil, invalid("the clone needs a valid disk ID for slot %d", slot)
		}
		slots[slot] = true
	}
	if len(slots) != len(spec.Disks) {
		return nil, invalid("the clone names disk slots the snapshot does not have")
	}
	existing, err := h.listDatasets(ctx, target, false)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, fmt.Errorf("hostops: the clone's dataset %s already exists", target)
	}

	// The clone's properties are not copied from the origin, so every
	// one Skep relies on is set here: the owner, and volmode=dev, without
	// which the host would taste the guest's partition table. A clone has
	// no reservation: it is thin, whatever the source was.
	if _, err := h.run.Run(ctx, zfsBinary, "clone", "-o", "canmount=off", "-o", propVMID+"="+spec.TargetVMID,
		name+"@"+short, target); err != nil {
		return nil, err
	}
	order := make([]int, 0, len(slots))
	for slot := range slots {
		order = append(order, slot)
	}
	sort.Ints(order)
	for _, slot := range order {
		suffix := "/disk" + strconv.Itoa(slot)
		if _, err := h.run.Run(ctx, zfsBinary, "clone", "-o", "volmode=dev",
			"-o", propVMID+"="+spec.TargetVMID, "-o", propDiskID+"="+spec.Disks[slot],
			name+suffix+"@"+short, target+suffix); err != nil {
			return nil, err
		}
	}
	return h.listDatasets(ctx, target, true)
}
