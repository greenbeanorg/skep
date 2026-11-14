package agentclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// VMs returns a page of VMs.
func (c *Client) VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var list models.VMList
	err := c.do(ctx, http.MethodGet, "/vms", q, nil, &list)
	return list, err
}

// VM returns one VM.
func (c *Client) VM(ctx context.Context, id string) (models.VM, error) {
	var vm models.VM
	err := c.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(id), nil, nil, &vm)
	return vm, err
}

// CreateVM asks the agent to create a VM for the caller in ctx. The
// caller's idempotency key, if any, goes with the request.
func (c *Client) CreateVM(ctx context.Context, req models.VMCreate) (models.VMCreated, error) {
	var created models.VMCreated
	err := c.do(ctx, http.MethodPost, "/vms", nil, req, &created)
	return created, err
}

// UpdateVM asks the agent to update a VM and returns the task.
func (c *Client) UpdateVM(ctx context.Context, id string, upd models.VMUpdate) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPut, "/vms/"+url.PathEscape(id), nil, upd, &task)
	return task, err
}

// DeleteVM asks the agent to delete a VM and returns the task.
func (c *Client) DeleteVM(ctx context.Context, id string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodDelete, "/vms/"+url.PathEscape(id), nil, nil, &task)
	return task, err
}

// VMAction asks the agent to start, stop, force-stop, or restart a VM, and
// returns the task.
func (c *Client) VMAction(ctx context.Context, id string, act models.VMAction) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPost, "/vms/"+url.PathEscape(id)+"/actions", nil, act, &task)
	return task, err
}

// StoragePools returns the allow-listed pools as the agent observes them.
func (c *Client) StoragePools(ctx context.Context) (models.StoragePoolList, error) {
	var list models.StoragePoolList
	err := c.do(ctx, http.MethodGet, "/storage/pools", nil, nil, &list)
	return list, err
}

// Media returns the listing of the media directory.
func (c *Client) Media(ctx context.Context) (models.MediaList, error) {
	var list models.MediaList
	err := c.do(ctx, http.MethodGet, "/storage/media", nil, nil, &list)
	return list, err
}

// StorageDatasets returns a page of the storage inventory.
func (c *Client) StorageDatasets(ctx context.Context, opts models.DatasetListOptions) (models.DatasetList, error) {
	q := url.Values{}
	if opts.Pool != "" {
		q.Set("pool", opts.Pool)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var list models.DatasetList
	err := c.do(ctx, http.MethodGet, "/storage/datasets", q, nil, &list)
	return list, err
}

func snapshotPath(vmID, snapID string) string {
	p := "/vms/" + url.PathEscape(vmID) + "/snapshots"
	if snapID != "" {
		p += "/" + url.PathEscape(snapID)
	}
	return p
}

// Snapshots returns a VM's snapshots, oldest first.
func (c *Client) Snapshots(ctx context.Context, vmID string) (models.SnapshotList, error) {
	var list models.SnapshotList
	err := c.do(ctx, http.MethodGet, snapshotPath(vmID, ""), nil, nil, &list)
	return list, err
}

// Snapshot returns one snapshot of a VM.
func (c *Client) Snapshot(ctx context.Context, vmID, id string) (models.Snapshot, error) {
	var snap models.Snapshot
	err := c.do(ctx, http.MethodGet, snapshotPath(vmID, id), nil, nil, &snap)
	return snap, err
}

// CreateSnapshot asks the agent to snapshot a VM.
func (c *Client) CreateSnapshot(ctx context.Context, vmID string, req models.SnapshotCreate) (models.SnapshotCreated, error) {
	var created models.SnapshotCreated
	err := c.do(ctx, http.MethodPost, snapshotPath(vmID, ""), nil, req, &created)
	return created, err
}

// RollbackSnapshot asks the agent to roll a VM back to a snapshot.
func (c *Client) RollbackSnapshot(ctx context.Context, vmID, id string, req models.SnapshotRollback) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPost, snapshotPath(vmID, id)+"/rollback", nil, req, &task)
	return task, err
}

// DeleteSnapshot asks the agent to delete a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, vmID, id string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodDelete, snapshotPath(vmID, id), nil, nil, &task)
	return task, err
}

// CloneVM asks the agent for a linked clone of a VM.
func (c *Client) CloneVM(ctx context.Context, vmID string, req models.VMClone) (models.VMCreated, error) {
	var created models.VMCreated
	err := c.do(ctx, http.MethodPost, "/vms/"+url.PathEscape(vmID)+"/clone", nil, req, &created)
	return created, err
}
