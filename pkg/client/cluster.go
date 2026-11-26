package client

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// RemoveTimeout is the client timeout a removal needs: the server may take
// up to 90 s to answer it (step 8c-2). Pass it as Options.Timeout.
const RemoveTimeout = 2 * time.Minute

// Cluster returns this node's view of its cluster.
func (c *Client) Cluster(ctx context.Context) (models.Cluster, error) {
	var out models.Cluster
	err := c.do(ctx, http.MethodGet, "/cluster", nil, nil, &out)
	return out, err
}

// ClusterGuests returns the cluster's guest inventory (step 8d-1):
// system:read, vm:read and container:read.
func (c *Client) ClusterGuests(ctx context.Context) (models.ClusterGuestList, error) {
	var out models.ClusterGuestList
	err := c.do(ctx, http.MethodGet, "/cluster/guests", nil, nil, &out)
	return out, err
}

// AdmitHost admits a host by the line "skep-cluster join-request" printed
// on it (cluster:manage).
func (c *Client) AdmitHost(ctx context.Context, request string) (models.ClusterAdmission, error) {
	var out models.ClusterAdmission
	err := c.do(ctx, http.MethodPost, "/cluster/admissions", nil, models.ClusterAdmitRequest{Request: request}, &out)
	return out, err
}

// WithdrawAdmission withdraws the pending admission of a host by its name
// (cluster:manage).
func (c *Client) WithdrawAdmission(ctx context.Context, name string) (models.ClusterWithdrawal, error) {
	var out models.ClusterWithdrawal
	err := c.do(ctx, http.MethodDelete, "/cluster/admissions/"+url.PathEscape(name), nil, nil, &out)
	return out, err
}

// RemoveMember removes a member by its name; clusterID must be the
// cluster's (cluster:manage). Use a client made with RemoveTimeout.
func (c *Client) RemoveMember(ctx context.Context, name, clusterID string) (models.ClusterRemoval, error) {
	var out models.ClusterRemoval
	err := c.do(ctx, http.MethodPost, "/cluster/members/"+url.PathEscape(name)+"/remove", nil, models.ClusterRemoveRequest{ClusterID: clusterID}, &out)
	return out, err
}
