package internalapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeStorage struct{ got models.DatasetListOptions }

func (f *fakeStorage) Datasets(_ context.Context, opts models.DatasetListOptions) (models.DatasetList, error) {
	f.got = opts
	if opts.Pool == "zroot" {
		return models.DatasetList{}, &models.Error{Code: models.CodePoolNotAllowed, Message: "no"}
	}
	return models.DatasetList{Items: []models.Dataset{{Name: "skeptest/skep", Pool: "skeptest", Type: "filesystem",
		Owner: models.DatasetOwner{Kind: models.DatasetOwnerSkep}}}, NextCursor: "next"}, nil
}

func TestStorageDatasetsThroughTheSocket(t *testing.T) {
	st := &fakeStorage{}
	path := startWith(t, &fakeBackend{}, me(), internalapi.Options{Storage: st})
	c := newClient(t, path)

	list, err := c.StorageDatasets(callerCtx(), models.DatasetListOptions{Pool: "skeptest", Limit: 5, Cursor: "abc"})
	if err != nil || len(list.Items) != 1 || list.NextCursor != "next" || list.Items[0].Owner.Kind != "skep" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if st.got != (models.DatasetListOptions{Pool: "skeptest", Limit: 5, Cursor: "abc"}) {
		t.Errorf("options = %+v", st.got)
	}
	_, err = c.StorageDatasets(callerCtx(), models.DatasetListOptions{Pool: "zroot"})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodePoolNotAllowed {
		t.Errorf("err = %v", err)
	}
	// Read-only.
	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}}
	if resp, _ := raw(t, path, http.MethodPost, agentproto.Prefix+"/storage/datasets", hdr); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", resp.StatusCode)
	}
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/storage/datasets?limit=0", hdr); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=0: %d", resp.StatusCode)
	}
}

type fakeMetrics struct{}

func (fakeMetrics) Metrics(context.Context) (models.Metrics, error) {
	return models.Metrics{NodeID: "node-1", CapacityWarnPercent: 80, CapacityFailPercent: 90,
		Pools: []models.PoolMetrics{{Name: "skeptest", Type: "zfs", CapacityPercent: 12.5, Capacity: models.CapacityOK}},
		VMs:   []models.VMMetrics{}}, nil
}

func TestMetricsThroughTheSocket(t *testing.T) {
	path := startWith(t, &fakeBackend{}, me(), internalapi.Options{Metrics: fakeMetrics{}})
	c := newClient(t, path)
	m, err := c.Metrics(callerCtx())
	if err != nil || m.NodeID != "node-1" || len(m.Pools) != 1 || m.Pools[0].CapacityPercent != 12.5 {
		t.Fatalf("metrics = %+v, %v", m, err)
	}
	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}}
	if resp, _ := raw(t, path, http.MethodPost, agentproto.Prefix+"/metrics", hdr); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", resp.StatusCode)
	}
	// Without a sampler the route does not exist.
	path = startWith(t, &fakeBackend{}, me(), internalapi.Options{})
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/metrics", hdr); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no sampler: %d", resp.StatusCode)
	}
}
