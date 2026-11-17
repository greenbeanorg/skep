package agentclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Containers returns a page of jails.
func (c *Client) Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error) {
	q := url.Values{}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var list models.ContainerList
	err := c.do(ctx, http.MethodGet, "/containers", q, nil, &list)
	return list, err
}

// Container returns one jail.
func (c *Client) Container(ctx context.Context, id string) (models.Container, error) {
	var ct models.Container
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id), nil, nil, &ct)
	return ct, err
}

// ContainerConsoleLog returns the end of a jail's console.log.
func (c *Client) ContainerConsoleLog(ctx context.Context, id string) (models.ContainerConsoleLog, error) {
	var log models.ContainerConsoleLog
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/console-log", nil, nil, &log)
	return log, err
}

// CreateContainer asks the agent to create a jail for the caller in ctx.
func (c *Client) CreateContainer(ctx context.Context, req models.ContainerCreate) (models.ContainerCreated, error) {
	var created models.ContainerCreated
	err := c.do(ctx, http.MethodPost, "/containers", nil, req, &created)
	return created, err
}

// UpdateContainer asks the agent to change a stopped jail and returns the
// task.
func (c *Client) UpdateContainer(ctx context.Context, id string, upd models.ContainerUpdate) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPut, "/containers/"+url.PathEscape(id), nil, upd, &task)
	return task, err
}

// DeleteContainer asks the agent to delete a jail and returns the task.
func (c *Client) DeleteContainer(ctx context.Context, id string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), nil, nil, &task)
	return task, err
}

// ContainerAction asks the agent to start, stop, or restart a jail, and
// returns the task.
func (c *Client) ContainerAction(ctx context.Context, id string, act models.ContainerAction) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/actions", nil, act, &task)
	return task, err
}

func containerSnapshotPath(id, snapID string) string {
	p := "/containers/" + url.PathEscape(id) + "/snapshots"
	if snapID != "" {
		p += "/" + url.PathEscape(snapID)
	}
	return p
}

// ContainerSnapshots returns a jail's snapshots, oldest first.
func (c *Client) ContainerSnapshots(ctx context.Context, id string) (models.SnapshotList, error) {
	var list models.SnapshotList
	err := c.do(ctx, http.MethodGet, containerSnapshotPath(id, ""), nil, nil, &list)
	return list, err
}

// ContainerSnapshot returns one snapshot of a jail.
func (c *Client) ContainerSnapshot(ctx context.Context, id, snapID string) (models.Snapshot, error) {
	var snap models.Snapshot
	err := c.do(ctx, http.MethodGet, containerSnapshotPath(id, snapID), nil, nil, &snap)
	return snap, err
}

// CreateContainerSnapshot asks the agent to snapshot a jail.
func (c *Client) CreateContainerSnapshot(ctx context.Context, id string, req models.SnapshotCreate) (models.SnapshotCreated, error) {
	var created models.SnapshotCreated
	err := c.do(ctx, http.MethodPost, containerSnapshotPath(id, ""), nil, req, &created)
	return created, err
}

// RollbackContainerSnapshot asks the agent to roll a jail back.
func (c *Client) RollbackContainerSnapshot(ctx context.Context, id, snapID string, req models.SnapshotRollback) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPost, containerSnapshotPath(id, snapID)+"/rollback", nil, req, &task)
	return task, err
}

// DeleteContainerSnapshot asks the agent to delete a jail snapshot.
func (c *Client) DeleteContainerSnapshot(ctx context.Context, id, snapID string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodDelete, containerSnapshotPath(id, snapID), nil, nil, &task)
	return task, err
}

// CloneContainer asks the agent for a linked clone of a jail.
func (c *Client) CloneContainer(ctx context.Context, id string, req models.ContainerClone) (models.ContainerCreated, error) {
	var created models.ContainerCreated
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/clone", nil, req, &created)
	return created, err
}
