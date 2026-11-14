package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// VMs returns one page of VMs, ordered by name.
func (c *Client) VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var out models.VMList
	return out, c.do(ctx, http.MethodGet, "/vms", q, nil, &out)
}

// VM returns one VM.
func (c *Client) VM(ctx context.Context, id string) (models.VM, error) {
	var out models.VM
	return out, c.do(ctx, http.MethodGet, "/vms/"+url.PathEscape(id), nil, nil, &out)
}

// CreateVM creates a VM. The VM exists, not yet provisioned, when this
// returns; the task provisions it.
func (c *Client) CreateVM(ctx context.Context, req models.VMCreate) (models.VMCreated, error) {
	var out models.VMCreated
	return out, c.do(ctx, http.MethodPost, "/vms", nil, req, &out)
}

// DeleteVM deletes a stopped VM and its disks, and returns the task.
func (c *Client) DeleteVM(ctx context.Context, id string) (models.Task, error) {
	var out models.Task
	return out, c.do(ctx, http.MethodDelete, "/vms/"+url.PathEscape(id), nil, nil, &out)
}

// VMAction starts, stops, force-stops, or restarts a VM, and returns the
// task.
func (c *Client) VMAction(ctx context.Context, id string, act models.VMAction) (models.Task, error) {
	var out models.Task
	return out, c.do(ctx, http.MethodPost, "/vms/"+url.PathEscape(id)+"/actions", nil, act, &out)
}
