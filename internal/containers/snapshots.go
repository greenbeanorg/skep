package containers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Snapshots, rollback, and linked clones of jails (ADR 0010 decisions 2,
// 3, and 13; amendments j and k). A jail's root is one dataset, so a
// snapshot is one zfs snapshot of it, through the same hostops calls as a
// VM's. Every task holds the jail as its resource, so a snapshot cannot
// race a start, an update, a rollback, or a delete of the same jail. A
// clone holds its source; the clone itself is a new, unprovisioned
// container that cannot be deleted while the clone task runs.
//
// started_once (decision 13) is never reset: a rollback keeps it, and a
// clone inherits its source's, since the snapshot's tree may have run
// code. A clone whose tree has run code is never written into.

func (s *Service) snapshotDefinitions() []tasks.Definition {
	return []tasks.Definition{
		{Type: models.TaskContainerSnapshotCreate, Cancellable: true, Timeout: time.Minute, Run: s.runSnapshotCreate},
		{Type: models.TaskContainerSnapshotRollback, Timeout: 5 * time.Minute, Run: s.runSnapshotRollback},
		{Type: models.TaskContainerSnapshotDelete, Timeout: 5 * time.Minute, Run: s.runSnapshotDelete},
		{Type: models.TaskContainerClone, Timeout: 10 * time.Minute, Run: s.runClone},
	}
}

func jailOwner(id string) hostops.Owner { return hostops.Owner{Kind: hostops.OwnerContainer, ID: id} }

// maxSnapshotDescription bounds a snapshot's description, as the schema
// does.
const maxSnapshotDescription = 1024

var errSnapshotNotProvisioned = &models.Error{Code: models.CodeSnapshotNotProvisioned,
	Message: "the snapshot was never completed; it can only be deleted"}

func validSnapshotRequest(name, description string) error {
	switch {
	case !nameRE.MatchString(name):
		return models.Invalid("a snapshot name is 1 to 63 letters, digits, '.', '_', or '-', starting with a letter or digit")
	case len(description) > maxSnapshotDescription:
		return models.Invalid(fmt.Sprintf("the description is longer than %d bytes", maxSnapshotDescription))
	}
	return nil
}

// snapshotPayload is the payload of every snapshot task. Request is what
// the user sent, normalised, so a replay with the same idempotency key is
// recognised although the generated ID differs.
type snapshotPayload struct {
	Request      json.RawMessage `json:"request,omitempty"`
	SnapshotID   string          `json:"snapshot_id"`
	DestroyNewer bool            `json:"destroy_newer,omitempty"`
}

// clonePayload is the payload of container.clone.
type clonePayload struct {
	Request           json.RawMessage `json:"request"`
	TargetContainerID string          `json:"target_container_id"`
	SnapshotID        string          `json:"snapshot_id"`
	// TakeSnapshot is set when the request named no snapshot: the task
	// takes one first and leaves it behind.
	TakeSnapshot bool `json:"take_snapshot,omitempty"`
}

func decodeSnapshotPayload(task tasks.Task) (snapshotPayload, error) {
	var p snapshotPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.SnapshotID) {
		return p, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a snapshot request"}
	}
	return p, nil
}

// provisioned returns a container that container.create (or
// container.clone) has finished.
func (s *Service) provisioned(ctx context.Context, id string) (models.Container, error) {
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Container{}, err
	}
	if !c.Provisioned {
		return models.Container{}, notProvisioned(c)
	}
	return c, nil
}

// stopped returns a provisioned container whose jail is not running.
func (s *Service) stopped(ctx context.Context, id string) (models.Container, error) {
	c, err := s.provisioned(ctx, id)
	if err != nil {
		return models.Container{}, err
	}
	if running, err := s.isRunning(ctx, id); err != nil {
		return models.Container{}, err
	} else if running {
		return models.Container{}, notStopped(c)
	}
	return c, nil
}

// consistency is what a snapshot taken now would be.
func (s *Service) consistency(ctx context.Context, id string) (string, error) {
	running, err := s.isRunning(ctx, id)
	if err != nil {
		return "", err
	}
	if running {
		return models.SnapshotCrashConsistent, nil
	}
	return models.SnapshotConsistent, nil
}

// Snapshots lists a jail's snapshots, oldest first, with what ZFS shows
// of each now.
func (s *Service) Snapshots(ctx context.Context, id string) (models.SnapshotList, error) {
	if !hostops.ValidID(id) {
		return models.SnapshotList{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.SnapshotList{}, err
	}
	list, err := s.snaps.List(ctx, models.ContainerResource, c.ID)
	if err != nil {
		return models.SnapshotList{}, err
	}
	s.observeSnapshots(ctx, c, list)
	return models.SnapshotList{Items: list}, nil
}

// Snapshot returns one snapshot of a jail.
func (s *Service) Snapshot(ctx context.Context, id, snapID string) (models.Snapshot, error) {
	if !hostops.ValidID(id) || !hostops.ValidID(snapID) {
		return models.Snapshot{}, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	}
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Snapshot{}, err
	}
	snap, err := s.snaps.Get(ctx, models.ContainerResource, c.ID, snapID)
	if err != nil {
		return models.Snapshot{}, err
	}
	list := []models.Snapshot{snap}
	s.observeSnapshots(ctx, c, list)
	return list[0], nil
}

// observeSnapshots fills in and records what ZFS shows of the given
// snapshots. A failure to read is logged: the answer is then the last
// observation.
func (s *Service) observeSnapshots(ctx context.Context, c models.Container, list []models.Snapshot) {
	if len(list) == 0 {
		return
	}
	found, err := s.zfs.Snapshots(ctx, c.Pool, jailOwner(c.ID))
	if err != nil {
		s.log.Warn("containers: could not read the snapshots of a jail", "container_id", c.ID, "error", err)
		return
	}
	used := map[string]int64{}
	exists := map[string]bool{}
	for _, d := range found {
		if d.SnapshotID != "" {
			exists[d.SnapshotID] = true
			used[d.SnapshotID] += d.UsedBytes
		}
	}
	at := s.now().UTC()
	for i := range list {
		id := list[i].ID
		list[i].Observed = &models.SnapshotObserved{Exists: exists[id], UsedBytes: used[id], At: &at}
		if err := s.snaps.SaveObserved(ctx, id, exists[id], used[id], at); err != nil {
			s.log.Warn("containers: could not record an observed snapshot", "snapshot_id", id, "error", err)
		}
	}
}

// CreateSnapshot validates a snapshot request, reserves its row, and
// submits the task that takes it. A running jail's snapshot is
// crash_consistent.
func (s *Service) CreateSnapshot(ctx context.Context, id string, req models.SnapshotCreate, sub Submission) (models.SnapshotCreated, error) {
	if !hostops.ValidID(id) {
		return models.SnapshotCreated{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	refused := sub.request(models.TaskContainerSnapshotCreate, id, nil)
	if err := validSnapshotRequest(req.Name, req.Description); err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	}
	c, err := s.provisioned(ctx, id)
	if err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	}
	request, err := canonical(req)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, models.TaskContainerSnapshotCreate, request); err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p snapshotPayload
		_ = json.Unmarshal(existing.Payload, &p)
		snap, err := s.snaps.Get(ctx, models.ContainerResource, c.ID, p.SnapshotID)
		if errors.Is(err, models.ErrNotFound) {
			snap = models.Snapshot{ID: p.SnapshotID, ResourceKind: models.ContainerResource, ResourceID: c.ID,
				Name: req.Name, Description: req.Description, Clones: []models.SnapshotClone{}}
		} else if err != nil {
			return models.SnapshotCreated{}, err
		}
		return models.SnapshotCreated{Snapshot: snap, Task: existing.Public()}, nil
	}
	consistency, err := s.consistency(ctx, c.ID)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	snap := models.Snapshot{ResourceKind: models.ContainerResource, ResourceID: c.ID, Name: req.Name,
		Description: req.Description, Consistency: consistency, CreatedAt: s.now().UTC()}
	if snap.ID, err = ids.NewUUID(); err != nil {
		return models.SnapshotCreated{}, err
	}
	payload, err := canonical(snapshotPayload{Request: request, SnapshotID: snap.ID})
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	submit := sub.request(models.TaskContainerSnapshotCreate, c.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		return s.snaps.InsertTx(ctx, tx, snap, s.maxSnapshots)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	stored, err := s.snaps.Get(ctx, models.ContainerResource, c.ID, snap.ID)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	return models.SnapshotCreated{Snapshot: stored, Task: task.Public()}, nil
}

// SnapshotResult is the result of container.snapshot.create.
type SnapshotResult struct {
	ContainerID string        `json:"container_id"`
	SnapshotID  string        `json:"snapshot_id"`
	Name        string        `json:"name"`
	Consistency string        `json:"consistency"`
	Datasets    []DatasetInfo `json:"datasets"`
}

// AuditDetails implements tasks.Audited.
func (r SnapshotResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name, "consistency": r.Consistency,
		"snapshots_created": r.Datasets}
}

func datasetInfo(list []hostops.Dataset) []DatasetInfo {
	out := []DatasetInfo{}
	for _, d := range list {
		out = append(out, DatasetInfo{Name: d.Name, Type: d.Type, UsedBytes: d.UsedBytes})
	}
	return out
}

func (s *Service) runSnapshotCreate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	snap, err := s.snaps.Get(ctx, models.ContainerResource, task.ResourceID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if snap.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the snapshot was already taken"}
	}
	c, err := s.provisioned(ctx, task.ResourceID)
	if err != nil {
		s.dropSnapshotRow(snap.ID)
		return nil, failure(err)
	}
	made, consistency, err := s.takeSnapshot(ctx, c, snap.ID, report)
	if err != nil {
		s.dropSnapshotRow(snap.ID)
		return nil, failure(err)
	}
	s.log.Info("jail snapshot taken", "container_id", c.ID, "snapshot_id", snap.ID, "consistency", consistency)
	return SnapshotResult{ContainerID: c.ID, SnapshotID: snap.ID, Name: snap.Name, Consistency: consistency,
		Datasets: datasetInfo(made)}, nil
}

// takeSnapshot takes the ZFS snapshot and marks the row provisioned. It
// can be cancelled until the ZFS call; after that the call runs to its
// end. The row is the caller's to remove on failure.
func (s *Service) takeSnapshot(ctx context.Context, c models.Container, snapID string, report tasks.Reporter) ([]hostops.Dataset, string, error) {
	// Taken when the snapshot is, not when it was asked for.
	consistency, err := s.consistency(ctx, c.ID)
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	report.Progress("taking the ZFS snapshot", map[string]any{"pool": c.Pool, "consistency": consistency})
	made, err := s.zfs.Snapshot(context.WithoutCancel(ctx), c.Pool, jailOwner(c.ID), snapID)
	if err != nil {
		return nil, "", err
	}
	var used int64
	for _, d := range made {
		used += d.UsedBytes
	}
	if err := s.snaps.SetProvisioned(context.WithoutCancel(ctx), snapID, consistency, used, s.now().UTC()); err != nil {
		return nil, "", err
	}
	return made, consistency, nil
}

// dropSnapshotRow removes the row of a snapshot that was not taken. It
// uses its own context: the task's may be why it is running.
func (s *Service) dropSnapshotRow(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.snaps.Delete(ctx, id); err != nil {
		s.log.Error("containers: could not remove the row of a snapshot that was not taken", "snapshot_id", id, "error", err)
	}
}

// clonesError is snapshot_has_clones from the database's records.
func clonesError(clones []models.SnapshotClone) error {
	return &models.Error{Code: models.CodeSnapshotHasClones,
		Message: "a linked clone depends on the snapshot; delete the clone first",
		Details: map[string]any{"clones": clones}}
}

// checkRollback validates a rollback against the database: the jail is
// stopped and provisioned, the snapshot is complete, and the newer
// snapshots may be destroyed. ZFS checks again in the task.
func (s *Service) checkRollback(ctx context.Context, id, snapID string, destroyNewer bool) (models.Container, models.Snapshot, []models.Snapshot, error) {
	c, err := s.stopped(ctx, id)
	if err != nil {
		return models.Container{}, models.Snapshot{}, nil, err
	}
	list, err := s.snaps.List(ctx, models.ContainerResource, c.ID)
	if err != nil {
		return models.Container{}, models.Snapshot{}, nil, err
	}
	var target models.Snapshot
	var newer []models.Snapshot
	found := false
	for _, snap := range list {
		switch {
		case snap.ID == snapID:
			target, found = snap, true
		case found:
			newer = append(newer, snap)
		}
	}
	switch {
	case !found:
		return models.Container{}, models.Snapshot{}, nil, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	case !target.Provisioned:
		return models.Container{}, models.Snapshot{}, nil, errSnapshotNotProvisioned
	case len(newer) > 0 && !destroyNewer:
		ids := make([]string, len(newer))
		for i, n := range newer {
			ids[i] = n.ID
		}
		return models.Container{}, models.Snapshot{}, nil, &models.Error{Code: models.CodeSnapshotNotLatest,
			Message: "the snapshot is not the latest; rolling back to it destroys the newer ones, which needs destroy_newer",
			Details: map[string]any{"snapshot_id": snapID, "newer_snapshot_ids": ids}}
	}
	var clones []models.SnapshotClone
	for _, n := range newer {
		clones = append(clones, n.Clones...)
	}
	if len(clones) > 0 {
		return models.Container{}, models.Snapshot{}, nil, clonesError(clones)
	}
	return c, target, newer, nil
}

// RollbackSnapshot submits a container.snapshot.rollback task.
func (s *Service) RollbackSnapshot(ctx context.Context, id, snapID string, req models.SnapshotRollback, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) || !hostops.ValidID(snapID) {
		return models.Task{}, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	}
	payload, err := canonical(snapshotPayload{SnapshotID: snapID, DestroyNewer: req.DestroyNewer})
	if err != nil {
		return models.Task{}, err
	}
	submit := sub.request(models.TaskContainerSnapshotRollback, id, payload)
	if _, _, _, err := s.checkRollback(ctx, id, snapID, req.DestroyNewer); err != nil {
		return models.Task{}, s.refuse(ctx, submit, err)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// SnapshotRef names a snapshot in a task result.
type SnapshotRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// RollbackResult is the result of container.snapshot.rollback. Destroyed
// lists the newer snapshots destroyed with destroy_newer;
// DatasetsDestroyed the ZFS snapshots that were.
type RollbackResult struct {
	ContainerID       string        `json:"container_id"`
	SnapshotID        string        `json:"snapshot_id"`
	Name              string        `json:"name"`
	Destroyed         []SnapshotRef `json:"destroyed_snapshots"`
	DatasetsDestroyed []DatasetInfo `json:"datasets_destroyed"`
}

// AuditDetails implements tasks.Audited: every snapshot a rollback
// destroyed is on the record (decision 14).
func (r RollbackResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name,
		"snapshots_destroyed": r.Destroyed, "datasets_destroyed": r.DatasetsDestroyed}
}

func (s *Service) runSnapshotRollback(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	c, target, newer, err := s.checkRollback(ctx, task.ResourceID, p.SnapshotID, p.DestroyNewer)
	if err != nil {
		return nil, failure(err)
	}
	report.Progress("rolling the jail's root back", map[string]any{"snapshot_id": target.ID, "destroy_newer": p.DestroyNewer})
	destroyed, err := s.zfs.Rollback(ctx, c.Pool, jailOwner(c.ID), target.ID, p.DestroyNewer)
	if err != nil {
		return nil, failure(err)
	}
	// What ZFS destroyed decides which rows go; the database's list of
	// newer snapshots names them.
	names := map[string]string{}
	for _, n := range newer {
		names[n.ID] = n.Name
	}
	gone := []SnapshotRef{}
	seen := map[string]bool{}
	for _, d := range destroyed {
		if d.SnapshotID == "" || seen[d.SnapshotID] {
			continue
		}
		seen[d.SnapshotID] = true
		gone = append(gone, SnapshotRef{ID: d.SnapshotID, Name: names[d.SnapshotID]})
		if err := s.snaps.Delete(ctx, d.SnapshotID); err != nil {
			return nil, err
		}
	}
	// started_once stays as it is (decision 13): even a snapshot taken
	// before the first start does not make the tree writable again.
	s.log.Info("jail rolled back", "container_id", c.ID, "snapshot_id", target.ID, "destroyed", len(gone))
	return RollbackResult{ContainerID: c.ID, SnapshotID: target.ID, Name: target.Name, Destroyed: gone,
		DatasetsDestroyed: datasetInfo(destroyed)}, nil
}

// checkSnapshotDelete validates a snapshot delete: the snapshot exists and
// no clone depends on it. The jail may be running.
func (s *Service) checkSnapshotDelete(ctx context.Context, id, snapID string) (models.Container, models.Snapshot, error) {
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Container{}, models.Snapshot{}, err
	}
	snap, err := s.snaps.Get(ctx, models.ContainerResource, c.ID, snapID)
	if err != nil {
		return models.Container{}, models.Snapshot{}, err
	}
	if len(snap.Clones) > 0 {
		return models.Container{}, models.Snapshot{}, clonesError(snap.Clones)
	}
	return c, snap, nil
}

// DeleteSnapshot submits a container.snapshot.delete task.
func (s *Service) DeleteSnapshot(ctx context.Context, id, snapID string, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) || !hostops.ValidID(snapID) {
		return models.Task{}, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	}
	payload, err := canonical(snapshotPayload{SnapshotID: snapID})
	if err != nil {
		return models.Task{}, err
	}
	submit := sub.request(models.TaskContainerSnapshotDelete, id, payload)
	if _, _, err := s.checkSnapshotDelete(ctx, id, snapID); err != nil {
		return models.Task{}, s.refuse(ctx, submit, err)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// SnapshotDeleteResult is the result of container.snapshot.delete.
type SnapshotDeleteResult struct {
	ContainerID string        `json:"container_id"`
	SnapshotID  string        `json:"snapshot_id"`
	Name        string        `json:"name"`
	Datasets    []DatasetInfo `json:"datasets_destroyed"`
}

// AuditDetails implements tasks.Audited.
func (r SnapshotDeleteResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name, "datasets_destroyed": r.Datasets}
}

func (s *Service) runSnapshotDelete(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	c, snap, err := s.checkSnapshotDelete(ctx, task.ResourceID, p.SnapshotID)
	if err != nil {
		return nil, failure(err)
	}
	report.Progress("destroying the snapshot", nil)
	destroyed, err := s.zfs.DestroySnapshot(ctx, c.Pool, jailOwner(c.ID), snap.ID)
	if errors.Is(err, models.ErrNotFound) {
		// Not in ZFS (an interrupted snapshot task): only the row is
		// left to remove.
		destroyed, err = nil, nil
	}
	if err != nil {
		return nil, failure(err)
	}
	if err := s.snaps.Delete(ctx, snap.ID); err != nil {
		return nil, err
	}
	s.log.Info("jail snapshot deleted", "container_id", c.ID, "snapshot_id", snap.ID, "datasets", len(destroyed))
	return SnapshotDeleteResult{ContainerID: c.ID, SnapshotID: snap.ID, Name: snap.Name, Datasets: datasetInfo(destroyed)}, nil
}

// checkDeletable refuses to delete a jail a linked clone depends on, and a
// clone a container.clone task is still making.
func (s *Service) checkDeletable(ctx context.Context, c models.Container) error {
	clones, err := s.snaps.ResourceClones(ctx, models.ContainerResource, c.ID)
	if err != nil {
		return err
	}
	if len(clones) > 0 {
		return clonesError(clones)
	}
	if !c.Provisioned && c.ClonedFromSnapshotID != "" {
		busy, err := s.snaps.ContainerCloneInProgress(ctx, c.ID)
		if err != nil {
			return err
		}
		if busy {
			return &models.Error{Code: models.CodeResourceBusy,
				Message: "the clone task that makes this container has not finished"}
		}
	}
	return nil
}

// cloneSource validates the source of a clone and, when named, its
// snapshot.
func (s *Service) cloneSource(ctx context.Context, srcID, snapID string) (models.Container, error) {
	src, err := s.provisioned(ctx, srcID)
	if err != nil {
		return models.Container{}, err
	}
	if snapID != "" {
		if !hostops.ValidID(snapID) {
			return models.Container{}, models.Invalid("invalid snapshot ID")
		}
		snap, err := s.snaps.Get(ctx, models.ContainerResource, src.ID, snapID)
		if errors.Is(err, models.ErrNotFound) {
			return models.Container{}, models.Invalid("the source container has no snapshot " + snapID)
		}
		if err != nil {
			return models.Container{}, err
		}
		if !snap.Provisioned {
			return models.Container{}, errSnapshotNotProvisioned
		}
	}
	return src, nil
}

// cloneOf builds the clone of a jail: the source's template, options,
// limits, SSH key, and NIC slots, with a new name, new NIC IDs, and new
// MAC addresses. The NICs keep the source's bridges and addressing except
// where the request changes them (amendment k):
//
//   - a source with a static address that has started cannot be cloned,
//     with or without new addressing (container_started);
//   - otherwise every NIC of the source with a static address needs new
//     addressing in the request (clone_addressing_required), or the clone
//     would hold the source's address;
//   - new addressing or nameservers go into the clone's rc.conf and
//     resolv.conf, which Skep writes only into a tree that has never run
//     code (decision 13): a source that has started gives a clone whose
//     tree has, and the change is container_started.
func (s *Service) cloneOf(ctx context.Context, src models.Container, req models.ContainerClone) (models.Container, error) {
	if !nameRE.MatchString(req.Name) {
		return models.Container{}, models.Invalid("a container name is 1 to 63 letters, digits, '.', '_', or '-', starting with a letter or digit")
	}
	c := models.Container{Name: req.Name, Hostname: req.Hostname, TemplateID: src.TemplateID,
		TemplateName: src.TemplateName, Pool: src.Pool, Release: src.Release, Options: src.Options,
		Limits: src.Limits, SSHAuthorizedKey: src.SSHAuthorizedKey, StartedOnce: src.StartedOnce}
	if c.Hostname == "" {
		c.Hostname = req.Name
	}
	if !hostops.ValidHostname(c.Hostname) {
		return c, models.Invalid("the hostname must be a DNS name: labels of letters, digits, and hyphens, separated by dots" +
			" (a name with '_' needs an explicit hostname)")
	}
	if err := s.checkLimits(ctx, c.Limits, c.Options); err != nil {
		return c, err
	}
	if req.NICs != nil && len(req.NICs) != len(src.NICs) {
		return c, models.Invalid(fmt.Sprintf("a clone keeps its source's %d NICs: nics needs one entry per NIC, in slot order", len(src.NICs)))
	}
	specs := make([]models.ContainerNICSpec, len(src.NICs))
	for i, n := range src.NICs {
		v4 := n.IPv4
		specs[i] = models.ContainerNICSpec{Bridge: n.Bridge, IPv4: &v4}
		if req.NICs != nil {
			r := req.NICs[i]
			if r.Bridge != "" {
				specs[i].Bridge = r.Bridge
			}
			specs[i].MACAddress = r.MACAddress
			if r.IPv4 != nil {
				specs[i].IPv4 = r.IPv4
			}
		}
	}
	nameservers := src.Nameservers
	if req.Nameservers != nil {
		nameservers = req.Nameservers
	} else if !anyStatic(specs) {
		// DHCP everywhere now: dhclient writes resolv.conf.
		nameservers = nil
	}
	nics, ns, err := s.normalizeNICs(specs, nameservers, nil)
	if err != nil {
		return c, err
	}
	if err := s.checkMACsFree(ctx, "", nics); err != nil {
		return c, err
	}
	// A started source with a static address cannot be cloned whatever the
	// request says: the clone would hold its address, and new addressing
	// cannot be written into a tree that has run code (decision 13). The
	// state refuses it (409); asking for new addressing (422) would send
	// the client to a request that cannot succeed.
	var keep []int
	sourceStatic := false
	for i, n := range src.NICs {
		sourceStatic = sourceStatic || n.IPv4.Mode == models.IPv4Static
		if n.IPv4.Mode == models.IPv4Static && nics[i].IPv4.Mode == models.IPv4Static &&
			sameIP(n.IPv4.Address, nics[i].IPv4.Address) {
			keep = append(keep, n.Slot)
		}
	}
	if sourceStatic && src.StartedOnce {
		return c, &models.Error{Code: models.CodeContainerStarted,
			Message: "the source jail has a static address and has started: a clone would hold the same address, and Skep does not write new addressing into a tree that has run code (decision 13), so this jail cannot be cloned",
			Details: map[string]any{"container_id": src.ID}}
	}
	if len(keep) > 0 {
		return c, &models.Error{Code: models.CodeCloneAddressingRequired,
			Message: "the source has a static address on these NIC slots, and the clone would hold the same address; give each new addressing in nics",
			Details: map[string]any{"slots": keep}}
	}
	if (!sameAddressing(src.NICs, nics) || !slices.Equal(ns, src.Nameservers)) && src.StartedOnce {
		return c, &models.Error{Code: models.CodeContainerStarted,
			Message: "the source jail has started, so the clone's tree has run code and Skep does not write new addressing into it (decision 13); clone a snapshot of a jail that never started, or keep the source's addressing",
			Details: map[string]any{"container_id": src.ID}}
	}
	c.NICs, c.Nameservers = nics, ns
	return c, nil
}

func anyStatic(specs []models.ContainerNICSpec) bool {
	for _, s := range specs {
		if s.IPv4 != nil && s.IPv4.Mode == models.IPv4Static {
			return true
		}
	}
	return false
}

// CloneContainer validates a clone request, reserves the clone's row (and
// the snapshot's, when it takes one), and submits the container.clone
// task.
func (s *Service) CloneContainer(ctx context.Context, srcID string, req models.ContainerClone, sub Submission) (models.ContainerCreated, error) {
	if !hostops.ValidID(srcID) {
		return models.ContainerCreated{}, fmt.Errorf("container %q: %w", srcID, models.ErrNotFound)
	}
	refused := sub.request(models.TaskContainerClone, srcID, nil)
	src, err := s.cloneSource(ctx, srcID, req.SnapshotID)
	if err != nil {
		return models.ContainerCreated{}, s.refuse(ctx, refused, err)
	}
	c, err := s.cloneOf(ctx, src, req)
	if err != nil {
		return models.ContainerCreated{}, s.refuse(ctx, refused, err)
	}
	request, err := canonical(req)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, models.TaskContainerClone, request); err != nil {
		return models.ContainerCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p clonePayload
		_ = json.Unmarshal(existing.Payload, &p)
		got, err := s.store.Get(ctx, p.TargetContainerID)
		if errors.Is(err, models.ErrNotFound) {
			got = c
			got.ID = p.TargetContainerID
		} else if err != nil {
			return models.ContainerCreated{}, err
		}
		return models.ContainerCreated{Container: withInterfaces(got), Task: existing.Public()}, nil
	}
	if c.ID, err = s.newContainerID(ctx); err != nil {
		return models.ContainerCreated{}, err
	}
	c.CreatedAt = s.now().UTC()
	p := clonePayload{Request: request, TargetContainerID: c.ID, SnapshotID: req.SnapshotID}
	var snap models.Snapshot
	if p.SnapshotID == "" {
		p.TakeSnapshot = true
		consistency, err := s.consistency(ctx, src.ID)
		if err != nil {
			return models.ContainerCreated{}, err
		}
		snap = models.Snapshot{ResourceKind: models.ContainerResource, ResourceID: src.ID, Name: req.Name,
			Description: "taken for the clone " + req.Name, Consistency: consistency, CreatedAt: s.now().UTC()}
		if snap.ID, err = ids.NewUUID(); err != nil {
			return models.ContainerCreated{}, err
		}
		p.SnapshotID = snap.ID
	}
	c.ClonedFromSnapshotID = p.SnapshotID
	payload, err := canonical(p)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	submit := sub.request(models.TaskContainerClone, src.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		if p.TakeSnapshot {
			if err := s.snaps.InsertTx(ctx, tx, snap, s.maxSnapshots); err != nil {
				return err
			}
		}
		return s.store.InsertTx(ctx, tx, s.nodeID, c)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	stored, err := s.store.Get(ctx, c.ID)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	return models.ContainerCreated{Container: withInterfaces(stored), Task: task.Public()}, nil
}

// CloneResult is the result of container.clone.
type CloneResult struct {
	ContainerID       string `json:"container_id"`
	Name              string `json:"name"`
	SourceContainerID string `json:"source_container_id"`
	SnapshotID        string `json:"snapshot_id"`
	SnapshotTaken     bool   `json:"snapshot_taken"`
	Dataset           string `json:"dataset"`
	Origin            string `json:"origin"`
	// FilesWritten is true when Skep wrote the clone's rc.conf and
	// resolv.conf: only for a clone whose tree has never run code.
	FilesWritten bool `json:"files_written"`
}

// AuditDetails implements tasks.Audited.
func (r CloneResult) AuditDetails() map[string]any {
	return map[string]any{"clone_container_id": r.ContainerID, "clone_name": r.Name, "snapshot_id": r.SnapshotID,
		"snapshot_taken": r.SnapshotTaken, "dataset": r.Dataset, "origin": r.Origin, "files_written": r.FilesWritten}
}

func (s *Service) runClone(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p clonePayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.TargetContainerID) || !hostops.ValidID(p.SnapshotID) {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a clone request"}
	}
	target, err := s.store.Get(ctx, p.TargetContainerID)
	if err != nil {
		return nil, err
	}
	if target.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the clone is already provisioned"}
	}
	src, err := s.provisioned(ctx, task.ResourceID)
	if err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	if p.TakeSnapshot {
		if _, _, err := s.takeSnapshot(ctx, src, p.SnapshotID, report); err != nil {
			s.undoClone(target)
			s.dropSnapshotRow(p.SnapshotID)
			return nil, failure(err)
		}
	}
	if _, err := s.cloneSource(ctx, src.ID, p.SnapshotID); err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	report.Progress("cloning the jail's root", map[string]any{"snapshot_id": p.SnapshotID})
	ds, err := s.jails.CloneJail(ctx, src.Pool, src.ID, p.SnapshotID, target.ID)
	if err == nil && target.Limits.DiskQuota > 0 {
		// A clone does not inherit refquota.
		report.Progress("setting the disk quota", nil)
		err = s.jails.SetJailQuota(ctx, target.Pool, target.ID, int64(target.Limits.DiskQuota))
	}
	written := false
	if err == nil && !target.StartedOnce {
		// The tree has never run code (decision 13): Skep's files are
		// written for the clone's own addressing, as on a create.
		var files hostops.JailFiles
		if files, err = jailFiles(target); err == nil && !emptyFiles(files) {
			report.Progress("writing rc.conf, resolv.conf, and root's SSH key into the clone", nil)
			err = s.jails.ConfigureJail(ctx, target.Pool, target.ID, files)
			written = err == nil
		}
	}
	if err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.store.SetProvisioned(rctx, target.ID, s.now().UTC()); err != nil {
		return nil, err
	}
	s.log.Info("jail cloned", "container_id", target.ID, "source_container_id", src.ID, "snapshot_id", p.SnapshotID,
		"started_once", target.StartedOnce)
	return CloneResult{ContainerID: target.ID, Name: target.Name, SourceContainerID: src.ID, SnapshotID: p.SnapshotID,
		SnapshotTaken: p.TakeSnapshot, Dataset: ds.Name, Origin: ds.Origin, FilesWritten: written}, nil
}

// undoClone removes a clone that failed: its dataset, which carries its
// own ID, and then its row. If the dataset cannot be destroyed the row
// stays, unprovisioned, for the user to delete. A snapshot the task took
// stays: it is a valid snapshot of the source.
func (s *Service) undoClone(c models.Container) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.jails.DestroyJailRoot(ctx, c.Pool, c.ID); err != nil {
		s.log.Error("containers: the dataset of a failed clone could not be destroyed; the clone is left to delete", "container_id", c.ID, "error", err)
		return
	}
	if err := s.jails.RemoveJailRun(c.ID); err != nil {
		s.log.Warn("containers: could not remove a failed clone's runtime directory", "container_id", c.ID, "error", err)
	}
	if err := s.store.Delete(ctx, c.ID); err != nil {
		s.log.Error("containers: could not remove the row of a failed clone", "container_id", c.ID, "error", err)
	}
}
